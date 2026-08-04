# dalivim trace harness (mode=trace, G16).
#
# This program is written into the jail alongside the student source by the
# runner. It runs the student's entrypoint under sys.settrace and transcribes a
# BOUNDED, structured JSON execution trace (one snapshot per executed line of the
# student's own code) into the report file, for a Python-Tutor-style step-through
# visualizer.
#
# SECURITY MODEL — the student code and every runtime value it produces are
# HOSTILE. This harness runs in the SAME jail as run mode (empty netns, cgroup,
# rlimits, read-only rootfs) and adds its own hard bounds ON TOP:
#   * total steps are capped (DALIVIM_TRACE_MAX_STEPS);
#   * the report is capped by an in-harness byte budget AND, authoritatively,
#     re-capped by the runner (TraceTruncated);
#   * value serialization is done by safe_repr, which NEVER calls a value's own
#     __repr__/__str__/__iter__/items — an arbitrary object renders as
#     "<ClassName>" only, and a value that DERIVES from a built-in is rendered
#     through the BUILT-IN's own unbound slot (int.__repr__, dict.items, …), which
#     a subclass cannot override. So a malicious/expensive/huge __repr__ can never
#     run, and cyclic or enormous structures are bounded by depth/length/item caps;
#   * only frames belonging to the student's own files are recorded — stdlib and
#     this harness are never traced.
# The harness writes ONLY to the report file; student stdout/stderr flow through
# to the real fds so the runner's output-limit kill still protects the process.
#
# The harness is deliberately dependency-free (stdlib only) and defensive: any
# internal error while recording a step is swallowed and never crashes the traced
# program (the trace degrades, the run continues).

import io
import json
import os
import runpy
import sys
from types import FunctionType as _FunctionType

# ---- bounds (runner-controlled via env; all have safe fallbacks) -------------

def _int_env(name, default):
    try:
        v = int(os.environ.get(name, ""))
        return v if v > 0 else default
    except (ValueError, TypeError):
        return default

MAX_STEPS = _int_env("DALIVIM_TRACE_MAX_STEPS", 2500)
MAX_REPORT_BYTES = _int_env("DALIVIM_TRACE_MAX_REPORT_BYTES", 2_000_000)

# Fixed per-value / per-frame caps. These are NOT request-driven — they are the
# harness's own hostile-input discipline, edited deliberately here.
MAX_STR = 256          # chars kept from any single scalar/str rendering
MAX_DEPTH = 4          # container nesting depth walked by safe_repr
MAX_ITEMS = 30         # elements rendered per container
MAX_VARS = 60          # variables recorded per frame
MAX_STACK = 25         # call-stack frames recorded per step
MAX_REPR = 512         # hard ceiling on a single value's whole rendered string
MAX_HEAP_OBJECTS = 200 # object-graph boxes emitted per step (trace v2 / A0)

TARGET = os.environ.get("DALIVIM_TRACE_TARGET", "main.py")
ROOT = os.path.realpath(os.environ.get("DALIVIM_TRACE_ROOT", "."))
REPORT = os.environ.get("DALIVIM_TRACE_REPORT", "trace.json")
HARNESS_PATH = os.path.realpath(__file__)

# ---- stdout accounting -------------------------------------------------------
# We forward student stdout to the real stream (so the runner still captures it
# and its output-limit kill still applies) while counting characters, so each
# step can carry the cumulative length of output produced so far. The frontend
# slices the captured stdout to that length to show "output up to this step".

class _CountingStdout:
    def __init__(self, wrapped):
        self._wrapped = wrapped
        self.count = 0

    def write(self, s):
        try:
            self.count += len(s)
        except Exception:
            pass
        return self._wrapped.write(s)

    def __getattr__(self, name):
        return getattr(self._wrapped, name)

_stdout = _CountingStdout(sys.stdout)
sys.stdout = _stdout

# ---- bounded, side-effect-free value rendering -------------------------------

_SCALAR_TYPES = (int, float, bool, type(None))

def _clip(s):
    if len(s) > MAX_STR:
        return s[:MAX_STR] + "\u2026"  # ellipsis marker
    return s

def safe_repr(v, depth=0, seen=None):
    """Render v to a bounded string WITHOUT invoking v.__repr__/__str__ for
    unknown types. Only built-in scalar/container shapes are walked; anything
    else becomes "<ClassName>". Cyclic and oversized structures are bounded by
    depth, item count, and total length caps.

    SUBCLASSES OF BUILT-INS ARE STUDENT CODE. `isinstance` is true for them, so
    every branch below dispatches through the BUILT-IN's own unbound slot
    (`int.__repr__(v)`, not `str(v)`; `dict.items(v)`, not `v.items()`). A
    subclass cannot override those, and the value still renders truthfully —
    a `class Celsius(float)` shows its number, a `Counter` shows its pairs.

    Falling back to "<ClassName>" for anything deriving from a built-in would be
    safe too, but it would hide the value; going through the base slot is both
    safe AND honest. Measured before this rule: a `class Loud(int)` whose
    `__str__` returned "42" was DRAWN AS 42 while holding 5, and a `__str__`
    returning 200 MB was materialised in full before _clip could cut it."""
    try:
        if v is None or v is True or v is False:
            return repr(v)  # None/True/False: fixed, safe literals
        if isinstance(v, bool):
            # bool cannot be subclassed, so its repr is always the built-in's.
            return repr(v)
        if isinstance(v, int):
            # int() has no unbounded __repr__ risk beyond digit count; clip huge ints.
            return _clip(int.__repr__(v))
        if isinstance(v, float):
            return _clip(float.__repr__(v))
        if isinstance(v, str):
            # Slicing through the base slot both CLIPS and returns a plain `str`,
            # so the escaping below cannot reach an overridden `replace`.
            return _clip(_quote_str(str.__getitem__(v, slice(None, MAX_STR + 1))))
        if isinstance(v, (bytes, bytearray)):
            base = bytes if isinstance(v, bytes) else bytearray
            return _clip(bytes.__repr__(bytes(base.__getitem__(v, slice(None, MAX_STR)))))

        if seen is None:
            seen = set()
        vid = id(v)
        if vid in seen:
            return "<circular>"
        if depth >= MAX_DEPTH:
            return "<\u2026>"  # too deep

        if isinstance(v, (list, tuple, set, frozenset)):
            seen = seen | {vid}
            open_c, close_c = _bracket(v)
            parts = []
            for i, item in enumerate(_itens_base(v)):
                if i >= MAX_ITEMS:
                    parts.append("\u2026")
                    break
                parts.append(safe_repr(item, depth + 1, seen))
                if _sum_len(parts) > MAX_REPR:
                    parts.append("\u2026")
                    break
            return _clip_total(open_c + ", ".join(parts) + close_c)

        if isinstance(v, dict):
            seen = seen | {vid}
            parts = []
            for i, (k, val) in enumerate(_pares_base(v)):
                if i >= MAX_ITEMS:
                    parts.append("\u2026")
                    break
                parts.append(safe_repr(k, depth + 1, seen) + ": " + safe_repr(val, depth + 1, seen))
                if _sum_len(parts) > MAX_REPR:
                    parts.append("\u2026")
                    break
            return _clip_total("{" + ", ".join(parts) + "}")

        # Unknown object: NEVER call its __repr__. Show its class only.
        return "<" + type(v).__name__ + ">"
    except Exception:
        return "<unrepr>"

def _itens_base(v):
    """Os elementos de uma sequência embutida, pelo slot da BASE.

    `for x in v` chama `type(v).__iter__`, e uma subclasse do aluno pode
    sobrescrever isso — levantar, mutar, ou nunca terminar. `list.__iter__(v)` é
    o iterador do próprio built-in, que uma subclasse não alcança."""
    for base in (list, tuple, set, frozenset):
        if isinstance(v, base):
            return base.__iter__(v)
    return iter(())


def _pares_base(v):
    """Os pares de um dicionário embutido, pelo slot da base — mesmo motivo."""
    return dict.items(v)


def _quote_str(s):
    # A bounded, escaped single-line rendering; avoids control chars in JSON.
    s = s.replace("\\", "\\\\").replace("'", "\\'").replace("\n", "\\n").replace("\r", "\\r").replace("\t", "\\t")
    return "'" + s + "'"

def _bracket(v):
    if isinstance(v, tuple):
        return "(", ")"
    if isinstance(v, (set, frozenset)):
        return "{", "}"
    return "[", "]"

def _sum_len(parts):
    return sum(len(p) for p in parts)

def _clip_total(s):
    if len(s) > MAX_REPR:
        return s[:MAX_REPR] + "\u2026"
    return s

def type_name(v):
    try:
        return type(v).__name__
    except Exception:
        return "?"

# ---- object-graph serialization (trace v2 / A0) ------------------------------
# A faithful Python-Tutor diagram needs OBJECT IDENTITY, not just a repr: two
# names bound to one list must point at one box, mutation must be visible on the
# same box across steps, and cycles must be drawable. So alongside the v1
# {repr,type} rendering we ALSO emit, per step, a reference graph: every frame
# variable becomes an inline primitive OR a {"ref": id}, and a per-step "heap"
# maps each id to a typed box (list/dict/object/...). This is purely ADDITIVE —
# v1 consumers ignore the new fields.
#
# SECURITY: this walk keeps the same hostile-value discipline as safe_repr. It
# NEVER calls a value's __repr__/__str__ to expand a container; it reads
# STRUCTURE only (type checks, iteration, instance __dict__). Cycles terminate on
# a seen-set of ids; growth is bounded by MAX_HEAP_OBJECTS (boxes), MAX_ITEMS
# (cells per container), and MAX_VARS (object fields). Real id() values are
# remapped to a dense per-trace counter so no raw memory address is exposed.

# Boxable container/collection kinds we expand into the heap. Anything else with
# no recognized shape becomes an inline opaque "<ClassName>" primitive (never a
# ref), so we never chase an unknown object's attributes.
def _remap_id(idmap, real_id, kind=None):
    """Map a real id() to a dense id that is stable ACROSS STEPS.

    The map used to be rebuilt every step, so ids were dense per STEP, not per
    trace: after `del a` every surviving object was renumbered. That breaks the
    one thing the diagram exists to teach — identity — and makes a per-step diff
    meaningless.

    ADDRESS REUSE: CPython can hand a freed object's address to a new object.
    Two guards, in order of strength:
      1. the caller prunes the map to the previous step's live set, so only
         recently-alive objects can be matched at all;
      2. the type name is stored with the id, so a freed list whose address is
         reused by a dict mints a NEW id instead of inheriting the list's box.
    A same-type reuse between two steps can still inherit an id; the effect is
    cosmetic (the box reads as "changed" rather than "new") and never a leak —
    the id is a dense counter and no address is ever exposed.

    `#n` holds the counter so ids keep rising even as entries are pruned."""
    entry = idmap.get(real_id)
    if entry is not None and (kind is None or entry[1] == kind):
        return entry[0]
    counter = idmap.get("#n", 0) + 1
    idmap["#n"] = counter
    rid = str(counter)
    idmap[real_id] = (rid, kind)
    return rid

def _is_prim(v):
    return v is None or isinstance(v, (bool, int, float, str, bytes, bytearray))

def _prim_value(v):
    """Inline primitive descriptor {"prim": type, "text": rendered}."""
    return {"prim": type_name(v), "text": safe_repr(v)}

def _instance_fields(v):
    # Instance attributes without invoking any descriptor/__getattr__ side effects:
    # read the raw __dict__ only. Objects using __slots__ (no __dict__) render as
    # an empty-field box, which is honest rather than risky.
    try:
        d = vars(v)
    except TypeError:
        return None
    if not isinstance(d, dict):
        return None
    return d

def _value_ref(v, idmap, heap, queue):
    """Serialize v as an inline primitive, or register it on the heap and return
    a {"ref": id}. Enqueues newly-seen objects for expansion."""
    if _is_prim(v):
        return _prim_value(v)
    real = id(v)
    rid = _remap_id(idmap, real, type_name(v))
    if rid not in heap:
        if len(heap) < MAX_HEAP_OBJECTS:
            heap[rid] = None        # reserve the slot before expanding (cycle-safe)
            queue.append((rid, v))
        else:
            # O teto cortou esta caixa. A referência SOBREVIVE e não alcança nada
            # — para o aluno isso é uma variável cujo valor não aparece. Sem
            # registrar, o desenho fica incompleto sem ninguém poder dizer isso;
            # é a mesma flag que o driver do Odin passou a emitir.
            _state["heap_limit_hit"] = True
    return {"ref": rid}

def _expand_function(f, idmap, heap, queue):
    """Function box: the signature the student wrote plus the names it CAPTURED.
    `co_freevars` names the closed-over variables and `__closure__` holds their
    cells, in the same order; an empty cell (a name captured but not yet bound)
    is rendered as such instead of raising."""
    try:
        code = f.__code__
        params = list(code.co_varnames[: code.co_argcount + code.co_kwonlyargcount])[:MAX_ITEMS]
        closure = {}
        names = code.co_freevars or ()
        cells = f.__closure__ or ()
        for i, name in enumerate(names):
            if i >= MAX_VARS or i >= len(cells):
                break
            try:
                closure[name] = _value_ref(cells[i].cell_contents, idmap, heap, queue)
            except ValueError:
                # A cell exists but is empty: the name is captured and not yet
                # assigned. That is a real state, worth showing as itself.
                closure[name] = {"prim": "empty", "text": "(ainda não definido)"}
            except Exception:
                closure[name] = {"prim": "opaque", "text": "<ilegível>"}
        return {
            "kind": "function",
            "cls": getattr(f, "__name__", "?"),
            "params": params,
            "closure": closure,
            "truncated": len(names) > MAX_VARS,
        }
    except Exception:
        return {"kind": "opaque", "cls": "function"}


def _expand_object(v, idmap, heap, queue):
    """Build the typed heap box for v. Children are serialized via _value_ref,
    so nested objects are enqueued and shared/cyclic refs collapse to one box."""
    try:
        if isinstance(v, (list, tuple, set, frozenset)):
            kind = ("list" if isinstance(v, list)
                    else "tuple" if isinstance(v, tuple) else "set")
            items = []
            truncated = False
            # Pelo slot da base, nunca por `for item in v`: ver _itens_base. Um
            # `class Minha(list)` com `__iter__` próprio é código do ALUNO, e
            # expandir a caixa não pode ser um jeito de executá-lo.
            for i, item in enumerate(_itens_base(v)):
                if i >= MAX_ITEMS:
                    truncated = True
                    break
                items.append(_value_ref(item, idmap, heap, queue))
            return {"kind": kind, "items": items, "truncated": truncated}
        if isinstance(v, dict):
            entries = []
            truncated = False
            for i, (k, val) in enumerate(_pares_base(v)):
                if i >= MAX_ITEMS:
                    truncated = True
                    break
                entries.append([_value_ref(k, idmap, heap, queue),
                                _value_ref(val, idmap, heap, queue)])
            return {"kind": "dict", "entries": entries, "truncated": truncated}
        # A function is the object a student most needs DRAWN, not labelled:
        # a closure is only understandable as a box holding the captured names.
        # Falling through to `opaque` (as this did) turned `contador()` into a
        # grey box and made the hardest topic the tutor exists to teach invisible.
        # types.FunctionType is checked EXACTLY: a class instance can fake
        # __name__/__closure__, and reading those off an arbitrary object would be
        # calling into student code, which the harness never does.
        if type(v) is _FunctionType:
            return _expand_function(v, idmap, heap, queue)
        fields = _instance_fields(v)
        if fields is not None:
            out = {}
            truncated = False
            for i, (name, val) in enumerate(fields.items()):
                if i >= MAX_VARS:
                    truncated = True
                    break
                if not isinstance(name, str):
                    continue
                out[name] = _value_ref(val, idmap, heap, queue)
            return {"kind": "object", "cls": type_name(v), "fields": out,
                    "truncated": truncated}
        # Recognized-but-unexpandable (a module, a function, a __slots__ object
        # with nothing readable): a labelled leaf box, never chased further.
        return {"kind": "opaque", "cls": type_name(v)}
    except Exception:
        return {"kind": "opaque", "cls": "?"}

def _vars_graph(mapping, idmap, heap, queue, drop_globals=False):
    """Frame variables as inline-primitive-or-ref, the v2 sibling of _vars_of."""
    out = {}
    n = 0
    try:
        items = list(mapping.items())
    except Exception:
        return out
    for name, val in items:
        if n >= MAX_VARS:
            break
        try:
            if not isinstance(name, str):
                continue
            if drop_globals and (not _is_recordable_name(name)
                                 or _skip_global_value(val, keep_student_functions=True)):
                continue
            out[name] = _value_ref(val, idmap, heap, queue)
            n += 1
        except Exception:
            continue
    return out

def _drain_heap(idmap, heap, queue):
    """Expand every enqueued object until the queue drains or the box cap is hit.
    A reserved (None) slot beyond the cap is dropped so the heap only holds real
    boxes; the dangling ref renders as a bounded 'unknown' on the frontend."""
    while queue:
        rid, obj = queue.pop(0)
        if heap.get(rid) is not None:
            continue
        heap[rid] = _expand_object(obj, idmap, heap, queue)
    for rid in [k for k, box in heap.items() if box is None]:
        del heap[rid]

def _is_recordable_name(name):
    # Skip dunder names in globals (module machinery), keep everything else.
    return not (name.startswith("__") and name.endswith("__"))

def _skip_global_value(v, keep_student_functions=False):
    # Globals include imported modules, functions, and classes — noise for a
    # variables panel. Keep data-ish values only.
    #
    # The GRAPH (v2) makes one exception: a function the STUDENT defined in the
    # traced file. Drawing it is the whole point of a closure diagram — a
    # `contador()` that returns `incr` is unreadable if `incr` is invisible — and
    # it is exactly what Python Tutor shows in the Global frame. Imported
    # functions, builtins, modules and classes stay filtered: those are the noise
    # this guard exists for. The v1 variables table keeps its old behaviour, so
    # this widens nothing that already ships.
    import types as _t
    if keep_student_functions and type(v) is _t.FunctionType:
        try:
            return v.__code__.co_filename != TARGET
        except Exception:
            return True
    return isinstance(v, (_t.ModuleType, _t.FunctionType, _t.BuiltinFunctionType,
                          _t.MethodType, type))

def _vars_of(mapping, drop_globals=False):
    out = {}
    n = 0
    try:
        items = list(mapping.items())
    except Exception:
        return out
    for name, val in items:
        if n >= MAX_VARS:
            break
        try:
            if not isinstance(name, str):
                continue
            if drop_globals and (not _is_recordable_name(name) or _skip_global_value(val)):
                continue
            out[name] = {"repr": safe_repr(val), "type": type_name(val)}
            n += 1
        except Exception:
            continue
    return out

# ---- frame filtering ---------------------------------------------------------

def _rel(path):
    try:
        rp = os.path.realpath(path)
        return os.path.relpath(rp, ROOT)
    except Exception:
        return path

_TARGET_REAL = os.path.realpath(TARGET)

def _is_student_frame(frame):
    try:
        raw = frame.f_code.co_filename
    except Exception:
        return False
    # Synthetic/frozen code objects ("<frozen runpy>", "<string>", "<stdin>") are
    # NOT real files. os.path.realpath would join them onto cwd and falsely match
    # the trace root, so reject them up front — only genuine paths are traced.
    if not raw or raw.startswith("<"):
        return False
    try:
        fn = os.path.realpath(raw)
    except Exception:
        return False
    if fn == HARNESS_PATH:
        return False
    return fn == _TARGET_REAL or fn.startswith(ROOT + os.sep)

def _build_stack(frame, idmap=None, heap=None, queue=None):
    """Outermost-first list of student frames. Each frame carries v1 `locals`
    ({repr,type}) AND, when an idmap/heap/queue is supplied, the v2 `vars`
    (inline-primitive-or-ref) that feed the shared per-step object graph — so an
    alias across two frames collapses to one heap box."""
    chain = []
    f = frame
    while f is not None and len(chain) < MAX_STACK * 4:
        if _is_student_frame(f):
            chain.append(f)
        f = f.f_back
    chain.reverse()
    if len(chain) > MAX_STACK:
        chain = chain[-MAX_STACK:]  # keep the innermost frames
    stack = []
    for f in chain:
        is_module = f.f_code.co_name == "<module>"
        entry = {
            "func": f.f_code.co_name,
            "file": _rel(f.f_code.co_filename),
            "line": f.f_lineno,
            "locals": _vars_of(f.f_locals, drop_globals=is_module),
        }
        if idmap is not None:
            entry["vars"] = _vars_graph(f.f_locals, idmap, heap, queue, drop_globals=is_module)
        stack.append(entry)
    return stack

# ---- the tracer -------------------------------------------------------------

def _envelope_pior_caso():
    """O documento SEM passo nenhum, no pior caso (com registro de quebra cheio).

    O orçamento de bytes contava só os passos, então ele fechava exatamente em
    MAX_REPORT_BYTES e o documento FINAL — com envelope — passava disso. Aí o teto
    externo do runner corta o arquivo no meio de um JSON, e o cliente recebe um
    documento que não dá para ler: o trace INTEIRO perdido, em vez de truncado.
    É a mesma forma de erro do resto deste trabalho — o limite, ao ser atingido,
    destruía a entrega em vez de degradá-la.

    Cobrar o pior caso custa alguns passos num trace enorme; não cobrar custa o
    trace todo."""
    return {
        "version": 1, "language": "python", "heap_encoding": "delta",
        "steps": [], "step_count": 0,
        "truncated": {"steps": True, "bytes": True, "heap": True},
        "limits": {"max_steps": MAX_STEPS, "max_report_bytes": MAX_REPORT_BYTES,
                   "max_heap_objects": MAX_HEAP_OBJECTS},
        "crash": {"type": "x" * 64, "message": "x" * (MAX_STR + 8),
                  "file": "x" * 128, "line": 999999999, "func": "x" * 128},
    }


_ENVELOPE_BYTES = len(json.dumps(_envelope_pior_caso(), ensure_ascii=False).encode("utf-8"))

_state = {
    "steps": [],
    # Começa cobrado do envelope: ver _envelope_pior_caso.
    "bytes": _ENVELOPE_BYTES,
    "step_limit_hit": False,
    "bytes_limit_hit": False,
    # Algum passo teve uma caixa cortada pelo teto de objetos (ver _value_ref).
    "heap_limit_hit": False,
    "stopped": False,
    # id() real → (id denso, nome do tipo), vivo pelo TRACE inteiro. Podado a cada
    # passo para o conjunto alcançável, que é o que limita o reuso de endereço.
    "idmap": {},
    # O último heap EMITIDO. O passo seguinte manda só a diferença contra ele.
    "prev_heap": None,
}

def _record(frame, event, arg):
    if _state["stopped"]:
        return
    if len(_state["steps"]) >= MAX_STEPS:
        _state["step_limit_hit"] = True
        _stop_tracing()
        return
    try:
        # One object graph per step, shared by every frame so aliases across
        # frames map to a single box. The idmap is now PER TRACE, so a box keeps
        # its id from the step it is born to the step it dies.
        idmap = _state["idmap"]
        heap, queue = {}, []
        top = _build_stack(frame, idmap, heap, queue)
        _drain_heap(idmap, heap, queue)
        cur = top[-1] if top else {"func": frame.f_code.co_name, "file": _rel(frame.f_code.co_filename), "line": frame.f_lineno}
        step = {
            "step": len(_state["steps"]),
            "event": event,
            "file": cur["file"],
            "line": cur["line"],
            "func": cur["func"],
            "stdout_len": _stdout.count,
            "stack": top,
        }
        # The RETURN VALUE, on the step where the frame returns. Python Tutor
        # shows it as a "Return value" row, and it is what closes the mental loop
        # of a call: without it the student watches a function finish and never
        # sees what it handed back. `arg` on a return event IS that value; it was
        # being discarded. It goes through the same bounded serializer as any
        # other value, so a huge/cyclic/hostile return is handled like the rest.
        if event == "return":
            try:
                step["retval"] = _value_ref(arg, idmap, heap, queue)
                _drain_heap(idmap, heap, queue)
            except Exception:
                pass
        if event == "exception" and isinstance(arg, tuple) and len(arg) >= 2:
            exc_type, exc_val = arg[0], arg[1]
            step["exc"] = {
                "type": getattr(exc_type, "__name__", str(exc_type)),
                "message": _clip(safe_repr(str(exc_val))),
            }
        # HEAP POR-TRACE. Repetir o heap inteiro em todo passo era a maior fonte
        # de bytes do documento — medido num laço de 120 iterações: 58% do
        # relatório era heap e 86% dos passos repetiam o anterior byte a byte.
        # Com o teto de 2 MB, é isso que corta um trace comum ao meio.
        # O primeiro passo com grafo manda o heap cheio; os seguintes mandam só
        # {set, del}, ou NADA quando nada mudou. É o mesmo conteúdo, e o "set"
        # ainda diz ao desenho O QUE mudou — que é o que destrava mostrar mutação.
        prev = _state["prev_heap"]
        if prev is None:
            step["heap"] = heap
        else:
            mudadas = {rid: box for rid, box in heap.items() if prev.get(rid) != box}
            sumidas = [rid for rid in prev if rid not in heap]
            if mudadas or sumidas:
                delta = {}
                if mudadas:
                    delta["set"] = mudadas
                if sumidas:
                    delta["del"] = sumidas
                step["heap_delta"] = delta

        # Approximate the report growth and stop before blowing the byte budget.
        # BYTES, não caracteres. O teto do runner conta bytes do arquivo; com
        # ensure_ascii=False um `…` (o próprio marcador de corte do harness) vale
        # 1 caractere e 3 bytes, e qualquer string com acento faz o mesmo. Medido:
        # um documento de 199 576 caracteres ocupava 200 100 bytes — 100 acima de
        # um teto de 200 000 —, e aí o teto externo corta o arquivo no meio de um
        # JSON. Contar caracteres num teto de bytes é o bug, e ele fica pior
        # exatamente onde este produto vive: texto em português.
        #
        # `+ 1` pela vírgula que este passo acrescenta ao array steps[].
        # O envelope é cobrado adiantado (ver _state["bytes"]).
        approx = len(json.dumps(step, ensure_ascii=False).encode("utf-8")) + 1
        if _state["bytes"] + approx > MAX_REPORT_BYTES:
            _state["bytes_limit_hit"] = True
            _stop_tracing()
            return
        _state["bytes"] += approx
        _state["steps"].append(step)
        # Só depois de o passo ENTRAR é que ele vira a base do próximo delta —
        # senão um passo descartado pelo teto deixaria o delta seguinte apontando
        # para um heap que nunca foi enviado.
        _state["prev_heap"] = heap
        # Poda: o mapa de ids guarda só o que estava vivo neste passo. É esse
        # corte que impede um endereço reaproveitado de herdar a caixa de um
        # objeto que morreu passos atrás.
        vivos = set(heap)
        _state["idmap"] = {
            k: v for k, v in idmap.items() if k == "#n" or (isinstance(v, tuple) and v[0] in vivos)
        }
    except Exception:
        # Never let a recording failure crash the traced program.
        return

def _global_trace(frame, event, arg):
    # Only descend into student frames; stdlib/harness frames are not traced,
    # which is both a big performance win and a correctness guard.
    if not _is_student_frame(frame):
        return None
    _record(frame, event, arg)
    return _local_trace

def _local_trace(frame, event, arg):
    _record(frame, event, arg)
    return _local_trace

def _stop_tracing():
    _state["stopped"] = True
    try:
        sys.settrace(None)
    except Exception:
        pass

# ---- run the student, catch its crash, emit the trace ------------------------

def main():
    crash = None
    sys.argv = [TARGET]
    sys.settrace(_global_trace)
    try:
        runpy.run_path(TARGET, run_name="__main__")
    except SystemExit as e:
        # A student sys.exit() is a normal completion, not a crash. Preserve code.
        _stop_tracing()
        code = e.code if isinstance(e.code, int) else (0 if e.code is None else 1)
        _emit(None)
        sys.exit(code)
    except BaseException as e:  # noqa: BLE001 — student exceptions are data here
        _stop_tracing()
        crash = _crash_from(e)
    finally:
        _stop_tracing()
    _emit(crash)
    sys.exit(1 if crash is not None else 0)

def _crash_from(e):
    # Walk the traceback to the last STUDENT frame — that is where it crashed.
    tb = e.__traceback__
    last = None
    while tb is not None:
        if _is_student_frame(tb.tb_frame):
            last = tb
        tb = tb.tb_next
    info = {
        "type": type(e).__name__,
        "message": _clip(safe_repr(str(e))),
    }
    if last is not None:
        info["file"] = _rel(last.tb_frame.f_code.co_filename)
        info["line"] = last.tb_lineno
        info["func"] = last.tb_frame.f_code.co_name
    return info

def _emit(crash):
    report = {
        # version stays 1: the object graph (per-step `heap` + per-frame `vars`)
        # is ADDITIVE, so v1 consumers keep working unchanged. It formalizes to
        # dalivim-trace-json@2 only when the v1 {repr,type} `locals` are removed.
        "version": 1,
        "language": "python",
        # Como o heap viaja: "delta" = o primeiro passo com grafo traz `heap`
        # cheio, os seguintes trazem `heap_delta` {set, del}, e um passo SEM
        # nenhum dos dois tem o heap idêntico ao anterior. Um consumidor que não
        # conheça a flag continua lendo `heap` onde ele aparece.
        "heap_encoding": "delta",
        "steps": _state["steps"],
        "step_count": len(_state["steps"]),
        "truncated": {
            "steps": _state["step_limit_hit"],
            "bytes": _state["bytes_limit_hit"],
            # O teto de CAIXAS. Diferente dos outros dois, ele não corta o trace:
            # ele corta o DESENHO, e a referência órfã que sobra é o que o aluno
            # vê como uma variável sem valor.
            "heap": _state["heap_limit_hit"],
        },
        "limits": {
            "max_steps": MAX_STEPS,
            "max_report_bytes": MAX_REPORT_BYTES,
            "max_heap_objects": MAX_HEAP_OBJECTS,
        },
    }
    if crash is not None:
        report["crash"] = crash
    try:
        data = json.dumps(report, ensure_ascii=False)
        # GARANTIA, não estimativa. O orçamento por passo é um freio BARATO — ele
        # evita construir um documento gigante — mas prever o overhead exato do
        # JSON (separadores do array, o envelope, o registro de quebra) erra por
        # alguns bytes, e medido erra por ~100 num trace de 326 passos.
        #
        # Alguns bytes bastam para o documento passar do teto EXTERNO do runner, e
        # aí o arquivo é cortado no meio de um JSON: o cliente recebe algo que não
        # dá para ler, o que é o trace INTEIRO perdido em vez de truncado. Aqui o
        # documento é medido de verdade e encolhe até caber.
        #
        # Corta pelo FIM: o começo é onde o aluno está olhando. Em proporção, para
        # convergir em poucas iterações em vez de uma por passo.
        while len(data.encode("utf-8")) > MAX_REPORT_BYTES and report["steps"]:
            sobra = max(1, len(report["steps"]) // 20)
            report["steps"] = report["steps"][:-sobra]
            report["step_count"] = len(report["steps"])
            report["truncated"]["bytes"] = True
            data = json.dumps(report, ensure_ascii=False)
    except Exception:
        data = json.dumps({"version": 1, "language": "python", "steps": [],
                           "step_count": 0, "error": "trace serialization failed"})
    # Atomic-ish write: the runner reads this back over the writable /sandbox bind.
    try:
        with io.open(REPORT, "w", encoding="utf-8") as f:
            f.write(data)
    except Exception:
        pass

if __name__ == "__main__":
    main()
