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
#     __repr__/__str__ — an arbitrary object renders as "<ClassName>" only, so a
#     malicious/expensive/huge __repr__ can never run and cyclic or enormous
#     structures are bounded by depth/length/item caps;
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
    depth, item count, and total length caps."""
    try:
        if v is None or v is True or v is False:
            return repr(v)  # None/True/False: fixed, safe literals
        if isinstance(v, bool):
            return repr(v)
        if isinstance(v, int):
            # int() has no unbounded __repr__ risk beyond digit count; clip huge ints.
            return _clip(str(v))
        if isinstance(v, float):
            return _clip(repr(v))
        if isinstance(v, str):
            return _clip(_quote_str(v))
        if isinstance(v, (bytes, bytearray)):
            return _clip(repr(bytes(v[:MAX_STR])))

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
            for i, item in enumerate(v):
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
            for i, (k, val) in enumerate(v.items()):
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

def _is_recordable_name(name):
    # Skip dunder names in globals (module machinery), keep everything else.
    return not (name.startswith("__") and name.endswith("__"))

def _skip_global_value(v):
    # Globals include imported modules, functions, and classes — noise for a
    # variables panel. Keep data-ish values only.
    import types as _t
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

def _build_stack(frame):
    """Outermost-first list of student frames, each with its locals."""
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
        stack.append({
            "func": f.f_code.co_name,
            "file": _rel(f.f_code.co_filename),
            "line": f.f_lineno,
            "locals": _vars_of(f.f_locals, drop_globals=is_module),
        })
    return stack

# ---- the tracer -------------------------------------------------------------

_state = {
    "steps": [],
    "bytes": 0,
    "step_limit_hit": False,
    "bytes_limit_hit": False,
    "stopped": False,
}

def _record(frame, event, arg):
    if _state["stopped"]:
        return
    if len(_state["steps"]) >= MAX_STEPS:
        _state["step_limit_hit"] = True
        _stop_tracing()
        return
    try:
        top = _build_stack(frame)
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
        if event == "exception" and isinstance(arg, tuple) and len(arg) >= 2:
            exc_type, exc_val = arg[0], arg[1]
            step["exc"] = {
                "type": getattr(exc_type, "__name__", str(exc_type)),
                "message": _clip(safe_repr(str(exc_val))),
            }
        # Approximate the report growth and stop before blowing the byte budget.
        approx = len(json.dumps(step, ensure_ascii=False))
        if _state["bytes"] + approx > MAX_REPORT_BYTES:
            _state["bytes_limit_hit"] = True
            _stop_tracing()
            return
        _state["bytes"] += approx
        _state["steps"].append(step)
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
        "version": 1,
        "language": "python",
        "steps": _state["steps"],
        "step_count": len(_state["steps"]),
        "truncated": {
            "steps": _state["step_limit_hit"],
            "bytes": _state["bytes_limit_hit"],
        },
        "limits": {
            "max_steps": MAX_STEPS,
            "max_report_bytes": MAX_REPORT_BYTES,
        },
    }
    if crash is not None:
        report["crash"] = crash
    try:
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
