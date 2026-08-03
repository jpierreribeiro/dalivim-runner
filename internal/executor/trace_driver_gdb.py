# Step-through trace driver for a COMPILED language (B.2), run by gdb --batch.
#
# It is the native-language sibling of trace_harness.py: same wire format
# (dalivim-trace-json@2), same bounds discipline, same hostility assumption —
# only the mechanism differs. Python has sys.settrace; a native binary has a
# debugger, so this drives gdb over a `-debug` build.
#
# EVERYTHING THE DEBUGGER READS IS HOSTILE. A native program can leave a pointer
# dangling, scribble a length field, or simply not have written a local yet, and
# the debugger will hand us whatever bytes are there. So:
#
#   * a BARE pointer is never dereferenced — only its address is reported. The
#     ONE exception is a container whose layout the language defines (an Odin
#     string/slice/dynamic array is {data,len[,cap]}): there we DO read through
#     `data`, but only after sanity-checking `len`, only up to MAX_ITEMS, and with
#     every element read guarded — a scribbled length or a dangling buffer yields
#     `opaque`, never a crash. Without this the student sees `{data: 0x5555…,
#     len: 3}` instead of `"ola"` or `[10, 20, 30]`, which is the implementation,
#     not the value;
#   * every value read is wrapped: any gdb error becomes `opaque`, never an
#     exception that kills the driver mid-trace;
#   * a variable is HIDDEN until execution passes its declaring line. DWARF scope
#     covers the whole frame, so before `x := 3` runs, `x` holds stack garbage —
#     showing it teaches the student a lie (measured: n = 140737488344096 before
#     its own initialiser). This is the single most important rule here;
#   * only frames in the STUDENT's file are recorded: `step` otherwise descends
#     into fmt.println and __memset_avx2, and those frames' values are noise read
#     from uninitialised memory;
#   * `context` is dropped — Odin injects it into every frame; it is the
#     compiler's, not the student's.
#
# Bounds mirror the Python harness: steps, heap boxes, container items, fields,
# string length and the total report size are all capped, and ids are remapped to
# a per-trace counter so no real address ever reaches the client.
#
# Config arrives by env (never argv): DALIVIM_TRACE_TARGET (the student source
# basename, which is also the file filter), DALIVIM_TRACE_REPORT (where to write),
# DALIVIM_TRACE_MAX_STEPS, DALIVIM_TRACE_MAX_REPORT_BYTES, DALIVIM_TRACE_ENTRY
# (the symbol to break on).

import json
import os

import gdb

TRACE_VERSION = 2
TRACE_FORMAT = "dalivim-trace-json@2"


def _int_env(name, default):
    try:
        v = int(os.environ.get(name, "") or default)
        return v if v > 0 else default
    except (TypeError, ValueError):
        return default


TARGET = os.environ.get("DALIVIM_TRACE_TARGET", "main.odin")
REPORT = os.environ.get("DALIVIM_TRACE_REPORT", "trace.json")
# The traced program's OWN streams. The inferior shares gdb's stdout, so without
# redirecting them the student's output arrives interleaved with the debugger's
# stepping chatter ("Breakpoint 1, main::main () at main.odin:13", every stepped
# source line, "Value returned is $1 = 2"). gdb's `set logging redirect` does NOT
# cover those stop announcements — measured. Redirecting the INFERIOR instead is
# exact: the runner reads these two files back as the student's stdout/stderr and
# drops gdb's stream entirely.
STDOUT_FILE = os.environ.get("DALIVIM_TRACE_STDOUT", "_dalivim_stdout.txt")
STDERR_FILE = os.environ.get("DALIVIM_TRACE_STDERR", "_dalivim_stderr.txt")
ENTRY = os.environ.get("DALIVIM_TRACE_ENTRY", "main::main")
LANGUAGE = os.environ.get("DALIVIM_TRACE_LANGUAGE", "odin")
MAX_STEPS = _int_env("DALIVIM_TRACE_MAX_STEPS", 2500)
MAX_REPORT_BYTES = _int_env("DALIVIM_TRACE_MAX_REPORT_BYTES", 2_000_000)

MAX_GDB_STEPS = MAX_STEPS * 20   # inner budget: stdlib frames we skip over
MAX_HEAP_OBJECTS = 200
MAX_FIELDS = 30
MAX_ITEMS = 30
MAX_STR = 256
MAX_DEPTH = 4
MAX_STACK = 25
# A length field read out of a hostile program is itself untrusted. Anything past
# this is treated as corrupt rather than walked — the cap that keeps a scribbled
# `len` from turning into a multi-gigabyte read.
MAX_SANE_LEN = 1_000_000

# Names the compiler injects into every frame; not the student's variables.
IMPLICIT_NAMES = {"context"}


def _clip(s):
    s = str(s)
    return s[:MAX_STR] + "…" if len(s) > MAX_STR else s


def _in_student_file(sal):
    """True when the stop is in the student's own source file."""
    try:
        return sal is not None and sal.symtab is not None and os.path.basename(sal.symtab.filename) == TARGET
    except Exception:
        return False


class Heap:
    """Per-step heap of typed boxes, ids remapped to a counter so a real address
    (ASLR) never reaches the client — the same choice trace_harness.py makes."""

    def __init__(self):
        self.boxes = {}
        self._ids = {}
        self._n = 0
        self.truncated = False

    def id_for(self, key):
        if key not in self._ids:
            if len(self._ids) >= MAX_HEAP_OBJECTS:
                return None
            self._n += 1
            self._ids[key] = str(self._n)
        return self._ids[key]


def _prim(t, text):
    return {"prim": str(t), "text": _clip(text)}


def _opaque(reason="ilegível"):
    return {"prim": "opaque", "text": "<%s>" % reason}


def _sane_len(v):
    """The `len` field of a container, or None when it is not believable. A
    native program can scribble it, so it is validated before it is used to
    drive any loop."""
    try:
        n = int(v["len"])
    except Exception:
        return None
    return n if 0 <= n <= MAX_SANE_LEN else None


def _odin_string(v):
    """Odin `string`/`cstring` -> the actual text. The layout is {data,len}; we
    read exactly `len` bytes and quote them, so the student sees "ola" instead of
    a pointer and a number."""
    n = _sane_len(v)
    if n is None:
        return _opaque("comprimento inválido")
    try:
        raw = v["data"].string(length=min(n, MAX_STR), errors="replace")
    except Exception:
        return _opaque("texto ilegível")
    text = '"%s"' % raw
    if n > MAX_STR:
        text = text[:-1] + '…"'
    return {"prim": "string", "text": text}


def _odin_sequence(v, t, heap, depth, kind_name):
    """Odin slice `[]T` / `[dynamic]T` -> a list box with real cells. Both are
    {data,len[,cap]}; `cap` is shown for a dynamic array because "grew to 8 slots
    holding 1" is exactly the thing a student needs to see."""
    n = _sane_len(v)
    if n is None:
        return _opaque("comprimento inválido")
    key = ("seq", str(v["data"]), str(t))
    rid = heap.id_for(key)
    if rid is None:
        heap.truncated = True
        return _opaque("limite de objetos")
    if rid not in heap.boxes:
        box = {"kind": "list", "cls": kind_name, "items": [], "truncated": n > MAX_ITEMS}
        heap.boxes[rid] = box
        items = []
        try:
            data = v["data"]
            for i in range(min(n, MAX_ITEMS)):
                try:
                    items.append(value_v2((data + i).dereference(), heap, depth + 1))
                except Exception:
                    items.append(_opaque())
        except Exception:
            box["truncated"] = True
        box["items"] = items
    return {"ref": rid}


def _odin_map(v, t, heap):
    """Odin `map[K]V`. Its `data` is the runtime's hash table, whose layout is
    internal and version-specific — walking it would be guesswork that breaks on
    the next toolchain bump. So the box is HONEST: the map's type and how many
    entries it holds, explicitly marked partial, instead of invented pairs."""
    rid = heap.id_for(("map", str(v["data"]), str(t)))
    if rid is None:
        heap.truncated = True
        return _opaque("limite de objetos")
    if rid not in heap.boxes:
        n = _sane_len(v)
        heap.boxes[rid] = {
            "kind": "object",
            "cls": str(t).replace("struct ", ""),
            "fields": {"len": _prim("int", n if n is not None else "?")},
            "truncated": True,
        }
    return {"ref": rid}


def value_v2(v, heap, depth=0):
    """gdb.Value -> trace v2 value: inline primitive or {"ref": id}. Never raises,
    never dereferences a pointer."""
    try:
        t = v.type.strip_typedefs()
        code = t.code
    except Exception:
        return _opaque()

    try:
        if code in (gdb.TYPE_CODE_INT, gdb.TYPE_CODE_BOOL, gdb.TYPE_CODE_FLT,
                    gdb.TYPE_CODE_ENUM, gdb.TYPE_CODE_CHAR):
            return _prim(t, v)

        if code == gdb.TYPE_CODE_PTR:
            # Address only. Dereferencing a dangling/scribbled pointer is exactly
            # how a hostile program would crash the tracer.
            try:
                return _prim("ptr", "0x%x" % int(v))
            except Exception:
                return _opaque("ponteiro inválido")

        if depth >= MAX_DEPTH:
            return _opaque("profundidade")

        # Odin's composite types are STRUCTS at the DWARF level ({data,len}), so
        # they must be recognised by NAME before the generic struct path — that
        # path would faithfully render the implementation and hide the value.
        if code == gdb.TYPE_CODE_STRUCT:
            tname = str(t).replace("struct ", "")
            if tname in ("string", "cstring"):
                return _odin_string(v)
            if tname.startswith("[]"):
                return _odin_sequence(v, t, heap, depth, tname)
            if tname.startswith("[dynamic]"):
                return _odin_sequence(v, t, heap, depth, tname)
            if tname.startswith("map["):
                return _odin_map(v, t, heap)

        if code == gdb.TYPE_CODE_STRUCT:
            key = ("s", str(v.address), str(t))
            rid = heap.id_for(key)
            if rid is None:
                heap.truncated = True
                return _opaque("limite de objetos")
            if rid not in heap.boxes:
                box = {"kind": "object", "cls": _clip(str(t).replace("struct ", "")), "fields": {}, "truncated": False}
                heap.boxes[rid] = box  # inserted BEFORE recursing: cycles terminate
                fields = {}
                for i, f in enumerate(t.fields()):
                    if i >= MAX_FIELDS:
                        box["truncated"] = True
                        break
                    if f.name is None:
                        continue
                    try:
                        fields[f.name] = value_v2(v[f.name], heap, depth + 1)
                    except Exception:
                        fields[f.name] = _opaque()
                box["fields"] = fields
            return {"ref": rid}

        if code == gdb.TYPE_CODE_ARRAY:
            key = ("a", str(v.address), str(t))
            rid = heap.id_for(key)
            if rid is None:
                heap.truncated = True
                return _opaque("limite de objetos")
            if rid not in heap.boxes:
                box = {"kind": "list", "items": [], "truncated": False}
                heap.boxes[rid] = box
                items = []
                try:
                    low, high = t.range()
                except Exception:
                    low, high = 0, -1
                n = 0
                for i in range(low, high + 1):
                    if n >= MAX_ITEMS:
                        box["truncated"] = True
                        break
                    try:
                        items.append(value_v2(v[i], heap, depth + 1))
                    except Exception:
                        items.append(_opaque())
                    n += 1
                box["items"] = items
            return {"ref": rid}

        return _opaque(t)
    except Exception:
        return _opaque()


def frame_vars(frame, heap, current_line):
    """Locals/args of one frame, EXCLUDING variables execution has not reached
    yet (see the header: DWARF scope != defined)."""
    out = {}
    try:
        block = frame.block()
    except Exception:
        return out
    # The function's own declaration line: arguments are only trustworthy once
    # the prologue past it has run.
    try:
        func_line = frame.function().line or 0
    except Exception:
        func_line = 0
    while block is not None:
        try:
            if block.is_global or block.is_static:
                break
            for sym in block:
                if not (sym.is_argument or sym.is_variable):
                    continue
                if sym.name in IMPLICIT_NAMES or sym.name in out:
                    continue
                # STRICTLY greater, not >=: while execution is stopped AT the
                # declaring line, that line has not run yet, so the slot still
                # holds stack garbage. `n := 3` on line 16 must stay hidden at
                # line 16 and appear at line 17. sym.line == 0 means "no line
                # info" — hide rather than show garbage.
                #
                # An ARGUMENT is bound by the caller, but not before the prologue
                # completes: at the function's own declaration line the registers
                # are not yet spilled and gdb reads nonsense (measured: a=1407…
                # at the `soma :: proc(...)` line, a=3 one line later). So the same
                # rule applies, against the FUNCTION's line.
                if sym.is_argument:
                    if current_line <= func_line:
                        continue
                else:
                    decl = getattr(sym, "line", 0) or 0
                    if decl == 0 or current_line <= decl:
                        continue
                try:
                    out[sym.name] = value_v2(sym.value(frame), heap)
                except Exception:
                    out[sym.name] = _opaque()
            block = block.superblock
        except Exception:
            break
    return out


def snapshot(step_no):
    """One trace v2 step: the student-visible stack, outermost first, plus the
    heap reachable from it."""
    heap = Heap()
    frames = []
    try:
        f = gdb.selected_frame()
    except Exception:
        return None
    while f is not None and len(frames) < MAX_STACK:
        frames.append(f)
        try:
            f = f.older()
        except Exception:
            break

    stack = []
    for fr in reversed(frames):
        try:
            sal = fr.find_sal()
            if not _in_student_file(sal):
                continue
            stack.append({
                "func": fr.name() or "?",
                "file": os.path.basename(sal.symtab.filename),
                "line": sal.line,
                "vars": frame_vars(fr, heap, sal.line),
            })
        except Exception:
            continue

    try:
        cur = gdb.selected_frame().find_sal()
        line = cur.line
        func = gdb.selected_frame().name() or "?"
    except Exception:
        return None

    return {
        "step": step_no,
        "event": "line",
        "file": TARGET,
        "line": line,
        "func": func,
        "stdout_len": 0,
        "stack": stack,
        "heap": heap.boxes,
    }


def _exec(cmd):
    try:
        gdb.execute(cmd, to_string=True)
        return True
    except gdb.error:
        return False


def collect():
    steps = []
    truncated_steps = False
    crash = None

    _exec("set confirm off")
    _exec("set pagination off")
    _exec("set height 0")
    # Send GDB's OWN output to /dev/null, keeping the inferior's untouched. The
    # student's stdout is a product surface: without this it arrives interleaved
    # with the debugger's stepping chatter —
    #   7
    #   Breakpoint 1, main::main () at main.odin:13
    #   13      n := 3
    # — and the program's real output drowns in it. `logging redirect` moves gdb's
    # stream only; the traced program keeps writing to the real stdout, which is
    # what the runner captures and shows.
    _exec("set logging file /dev/null")
    _exec("set logging redirect on")
    if not _exec("set logging enabled on"):
        _exec("set logging on")  # gdb < 12 spelling
    # A student program that segfaults must end the TRACE, not drop gdb into an
    # interactive stop we would then step forever.
    _exec("handle SIGSEGV stop nopass")

    if not _exec("break " + ENTRY):
        return steps, False, {"type": "InternalError", "message": "entrypoint %s não encontrado" % ENTRY}
    # Shell-style redirection on `run` is what separates the two streams.
    _exec("run > %s 2> %s" % (STDOUT_FILE, STDERR_FILE))

    inner = 0
    while len(steps) < MAX_STEPS and inner < MAX_GDB_STEPS:
        inner += 1
        try:
            frame = gdb.selected_frame()
            sal = frame.find_sal()
        except gdb.error:
            break  # program exited

        if _in_student_file(sal):
            snap = snapshot(len(steps))
            if snap is not None:
                steps.append(snap)
                # Size cap: stop while the document is still valid JSON.
                if len(json.dumps(steps)) > MAX_REPORT_BYTES:
                    steps.pop()
                    truncated_steps = True
                    break
            if not _exec("step"):
                break
        else:
            # Outside the student's file (stdlib, libc): leave without spending a
            # tutor step. `finish` fails at the outermost frame — then we are done.
            if not _exec("finish"):
                break

    if len(steps) >= MAX_STEPS:
        truncated_steps = True

    # Let the program RUN TO COMPLETION even when the step budget ran out: it is
    # still the student's program, its remaining output is theirs, and a process
    # killed mid-way never flushes its buffers (measured: an unfinished inferior
    # leaves the stdout file empty). Errors here just mean it already exited.
    _exec("continue")
    return steps, truncated_steps, crash


def main():
    try:
        steps, truncated, crash = collect()
    except Exception as e:  # a driver bug must not look like a student failure
        steps, truncated, crash = [], False, {"type": "InternalError", "message": _clip(e)}

    doc = {
        "version": TRACE_VERSION,
        "language": LANGUAGE,
        "steps": steps,
        "step_count": len(steps),
        "truncated": {"steps": truncated, "bytes": False},
    }
    if crash:
        doc["crash"] = crash

    blob = json.dumps(doc)
    if len(blob) > MAX_REPORT_BYTES:
        doc["steps"] = doc["steps"][: max(0, len(doc["steps"]) // 2)]
        doc["truncated"] = {"steps": truncated, "bytes": True}
        blob = json.dumps(doc)
    with open(REPORT, "w") as fh:
        fh.write(blob)


main()
gdb.execute("quit", to_string=True)
