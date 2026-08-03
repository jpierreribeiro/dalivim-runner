"""SPIKE B.2 — driver gdb que emite trace v2 (dalivim-trace-json@2) para um
binário Odin -debug. NÃO É CÓDIGO DE PRODUÇÃO: é a menor fatia que prova (ou
derruba) a viabilidade da abordagem DWARF+debugger, para a decisão gdb-vs-Valgrind.

Roda como: gdb --batch -x <este arquivo> ./prog

Restrições que o próprio spike descobriu e que qualquer versão real precisa ter:
  * só passos em ARQUIVO DO USUÁRIO — `step` cru desce em fmt::println e no
    __memset_avx2 da libc, o que não é o programa do aluno;
  * `context` é filtrado — o Odin injeta essa struct implícita (com ponteiros de
    procedure) em TODO frame; é ruído, não variável do aluno;
  * TODA leitura de valor é defensiva — em frames da stdlib o gdb devolveu
    strings de memória não inicializada ("LC_IDENTIFICATION=...") e levantou
    MemoryError. Ponteiro inválido vira `opaque`, nunca uma exceção que derruba
    o tracer (o requisito de hostilidade do estudo).
"""
import json
import sys

import gdb

MAX_STEPS = 40
MAX_EMIT = 6          # o spike só precisa provar 3-4 passos
MAX_DEPTH = 3
MAX_FIELDS = 20
IMPLICIT = {"context"}  # variáveis que o compilador injeta, não o aluno


def _user_file(sal):
    """True quando a parada está no ARQUIVO do aluno (não na stdlib/libc)."""
    if sal is None or sal.symtab is None:
        return False
    return sal.symtab.filename.endswith("prog.odin")


class Heap:
    """Boxes v2 com ids remapeados para um contador (nunca endereços reais —
    a mesma decisão do A0 de não vazar ASLR)."""

    def __init__(self):
        self.boxes = {}
        self.ids = {}
        self._n = 0

    def id_for(self, key):
        if key not in self.ids:
            self._n += 1
            self.ids[key] = str(self._n)
        return self.ids[key]


def prim(t, text):
    return {"prim": t, "text": text}


def value_v2(v, heap, depth=0):
    """Converte um gdb.Value em valor v2: primitivo inline ou {"ref": id}.
    Qualquer falha de leitura degrada para `opaque` — nunca propaga exceção."""
    try:
        t = v.type.strip_typedefs()
        code = t.code

        if code in (gdb.TYPE_CODE_INT, gdb.TYPE_CODE_BOOL, gdb.TYPE_CODE_FLT, gdb.TYPE_CODE_ENUM):
            return prim(str(t), str(v))

        if code == gdb.TYPE_CODE_PTR:
            # Ponteiro é HOSTIL por definição: só o endereço, sem desreferenciar.
            # Desreferenciar um ponteiro pendente/inválido é exatamente o que
            # derrubaria o tracer.
            return prim("ptr", "0x%x" % int(v))

        if code == gdb.TYPE_CODE_STRUCT and depth < MAX_DEPTH:
            key = ("struct", str(v.address) if v.address else id(v), str(t))
            rid = heap.id_for(key)
            if rid not in heap.boxes:
                heap.boxes[rid] = {"kind": "object", "cls": str(t), "fields": {}, "truncated": False}
                fields = {}
                n = 0
                for f in t.fields():
                    if n >= MAX_FIELDS:
                        heap.boxes[rid]["truncated"] = True
                        break
                    if f.name is None:
                        continue
                    try:
                        fields[f.name] = value_v2(v[f.name], heap, depth + 1)
                    except Exception:
                        fields[f.name] = {"prim": "opaque", "text": "<ilegível>"}
                    n += 1
                heap.boxes[rid]["fields"] = fields
            return {"ref": rid}

        return prim("opaque", "<%s>" % t)
    except Exception:
        return prim("opaque", "<ilegível>")


def frame_vars(frame, heap):
    out = {}
    try:
        block = frame.block()
    except Exception:
        return out
    while block is not None and not (block.is_global or block.is_static):
        for sym in block:
            if not (sym.is_argument or sym.is_variable):
                continue
            if sym.name in IMPLICIT or sym.name in out:
                continue
            try:
                out[sym.name] = value_v2(sym.value(frame), heap)
            except Exception:
                out[sym.name] = {"prim": "opaque", "text": "<ilegível>"}
        try:
            block = block.superblock
        except Exception:
            break
    return out


def snapshot(step_no):
    heap = Heap()
    stack = []
    f = gdb.selected_frame()
    frames = []
    while f is not None and len(frames) < 10:
        frames.append(f)
        try:
            f = f.older()
        except Exception:
            break
    for fr in reversed(frames):          # outermost primeiro, como o v1/v2
        try:
            sal = fr.find_sal()
            if not _user_file(sal):
                continue
            stack.append({
                "func": fr.name() or "?",
                "file": sal.symtab.filename,
                "line": sal.line,
                "vars": frame_vars(fr, heap),
            })
        except Exception:
            continue
    cur = gdb.selected_frame().find_sal()
    return {
        "step": step_no,
        "event": "line",
        "file": cur.symtab.filename if cur.symtab else "?",
        "line": cur.line,
        "func": gdb.selected_frame().name() or "?",
        "stdout_len": 0,
        "stack": stack,
        "heap": heap.boxes,
    }


def main():
    gdb.execute("set confirm off", to_string=True)
    gdb.execute("set pagination off", to_string=True)
    gdb.execute("break main::main", to_string=True)
    gdb.execute("run", to_string=True)

    steps = []
    for i in range(MAX_STEPS):
        if len(steps) >= MAX_EMIT:
            break
        try:
            sal = gdb.selected_frame().find_sal()
        except gdb.error:
            break
        if _user_file(sal):
            steps.append(snapshot(len(steps)))
            cmd = "step"
        else:
            # saiu do código do aluno: volta sem gastar passo do tutor
            cmd = "finish"
        try:
            gdb.execute(cmd, to_string=True)
        except gdb.error:
            break

    doc = {
        "version": 2,
        "language": "odin",
        "steps": steps,
        "step_count": len(steps),
        "truncated": {"steps": False, "bytes": False},
    }
    sys.stdout.write("===TRACE-V2===\n" + json.dumps(doc, indent=1) + "\n")


main()
gdb.execute("quit", to_string=True)
