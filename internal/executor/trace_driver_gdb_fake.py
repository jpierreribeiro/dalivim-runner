# Um `gdb` FALSO, só o bastante para rodar trace_driver_gdb.py de verdade.
#
# Existe porque o driver do Odin não tinha um único teste de comportamento: as
# garantias dele eram todas `strings.Contains` sobre o próprio fonte (odin_test.go),
# o que passa em qualquer refactor que preserve as strings e quebra em qualquer
# renomeação que não mude nada. O harness Python sempre teve o contrário — testes
# que rodam CPython de verdade —, e foi um deles que pegou o bug de identidade.
#
# A imagem de dev não tem gdb nem o toolchain do Odin, e o smoke on-target não
# roda no unit test. Então o que dá para fazer aqui é executar o driver contra um
# PROGRAMA ROTEIRIZADO: uma lista de paradas, cada uma com a pilha, as variáveis e
# as linhas em que elas foram declaradas. Isso não prova nada sobre o DWARF do
# Odin — nada aqui substitui o smoke —, mas prova toda a lógica que mora no
# driver: identidade de caixa, delta do heap, atribuição do valor devolvido,
# contagem de saída, tetos.
#
# O que NÃO é simulado, de propósito, para o falso não virar uma segunda
# implementação que concorda consigo mesma: memória de verdade, ASLR, DWARF,
# convenção de chamada. Ponteiros são inteiros e o alvo é um dicionário.

import sys
import types


class error(Exception):
    pass


TYPE_CODE_PTR = 1
TYPE_CODE_ARRAY = 2
TYPE_CODE_STRUCT = 3
TYPE_CODE_UNION = 4
TYPE_CODE_ENUM = 5
TYPE_CODE_FLT = 6
TYPE_CODE_INT = 7
TYPE_CODE_BOOL = 8
TYPE_CODE_CHAR = 9


class Type:
    def __init__(self, nome, code, campos=None, alvo=None, faixa=None):
        self._nome = nome
        self.code = code
        self._campos = campos or []       # [(nome, Type)]
        self._alvo = alvo
        self._faixa = faixa

    def __str__(self):
        return self._nome

    def strip_typedefs(self):
        return self

    def target(self):
        if self._alvo is None:
            raise error("sem alvo")
        return self._alvo

    def fields(self):
        return [types.SimpleNamespace(name=n, type=t) for n, t in self._campos]

    def range(self):
        if self._faixa is None:
            raise error("sem faixa")
        return self._faixa


INT = Type("int", TYPE_CODE_INT)


class Value:
    """Um valor do inferior. `bruto` é int para escalar/ponteiro, dict para
    struct, list para arranjo. `endereco` é o endereço FICTÍCIO dele."""

    def __init__(self, bruto, tipo, endereco=None, memoria=None):
        self.bruto = bruto
        self.type = tipo
        self.address = endereco
        self.memoria = memoria if memoria is not None else {}

    def __int__(self):
        if isinstance(self.bruto, int):
            return self.bruto
        raise error("não é inteiro")

    def __str__(self):
        return str(self.bruto)

    def __getitem__(self, nome):
        if isinstance(self.bruto, dict) and nome in self.bruto:
            return self.bruto[nome]
        if isinstance(self.bruto, list) and isinstance(nome, int):
            return self.bruto[nome]
        raise error("sem campo %r" % (nome,))

    def __add__(self, n):
        # Aritmética de ponteiro sobre um arranjo simulado: o "ponteiro" carrega
        # a lista de destino e um deslocamento.
        if not isinstance(self.bruto, tuple):
            raise error("não é ponteiro para dados")
        celulas, off = self.bruto
        return Value((celulas, off + n), self.type, memoria=self.memoria)

    def dereference(self):
        if isinstance(self.bruto, tuple):
            celulas, off = self.bruto
            if off >= len(celulas):
                raise error("fora da faixa")
            return celulas[off]
        if isinstance(self.bruto, int):
            alvo = self.memoria.get(self.bruto)
            if alvo is None:
                raise error("ponteiro pendurado")
            return alvo
        raise error("não desreferenciável")

    def string(self, length=None, errors=None):
        if isinstance(self.bruto, tuple):
            celulas, off = self.bruto
            texto = "".join(str(c.bruto) for c in celulas[off:])
            return texto[:length] if length is not None else texto
        raise error("não é texto")


class Symbol:
    def __init__(self, nome, valor, linha, argumento=False):
        self.name = nome
        self._valor = valor
        self.line = linha
        self.is_argument = argumento
        self.is_variable = not argumento

    def value(self, frame):
        return self._valor


class Block:
    def __init__(self, simbolos):
        self._simbolos = simbolos
        self.superblock = None
        self.is_global = False
        self.is_static = False

    def __iter__(self):
        return iter(self._simbolos)


class Symtab:
    def __init__(self, filename):
        self.filename = filename


class Sal:
    def __init__(self, arquivo, linha):
        self.symtab = Symtab(arquivo) if arquivo else None
        self.line = linha


class Frame:
    def __init__(self, func, linha, simbolos, arquivo, decl, sp, ret_pc, mais_velho):
        self._func = func
        self._linha = linha
        self._simbolos = simbolos
        self._arquivo = arquivo
        self._decl = decl
        self._sp = sp
        self._ret_pc = ret_pc
        self._mais_velho = mais_velho

    def name(self):
        return self._func

    def older(self):
        return self._mais_velho

    def find_sal(self):
        return Sal(self._arquivo, self._linha)

    def block(self):
        return Block(self._simbolos)

    def function(self):
        return types.SimpleNamespace(line=self._decl)

    def pc(self):
        # O pc de um quadro visto de baixo é o endereço de RETORNO dele — é isso
        # que o driver usa como metade da chave (ver _id_de_quadro).
        return self._ret_pc

    def read_register(self, nome):
        if nome == "sp":
            return self._sp
        raise error("registrador %s" % nome)


# ── o roteiro ────────────────────────────────────────────────────────────────

class Programa:
    """Um programa roteirizado: paradas em ordem, e o que sai no stdout em cada
    uma. Cada parada é uma lista de quadros do mais EXTERNO para o mais interno."""

    def __init__(self, paradas, arquivo="main.odin", saidas=None):
        self.paradas = paradas
        self.arquivo = arquivo
        self.saidas = saidas or {}
        self.i = -1
        self.terminou = False
        # id do quadro -> valor que ele devolveu, colhido enquanto ele vivia.
        self.retornos = {}


_prog = None
_saida_arquivo = None


class _Evento:
    def __init__(self):
        self._fs = []

    def connect(self, f):
        self._fs.append(f)

    def emitir(self, evt):
        for f in self._fs:
            f(evt)


class _Eventos:
    def __init__(self):
        self.exited = _Evento()
        self.stop = _Evento()


events = _Eventos()


class FinishBreakpoint:
    """O suficiente do contrato: arma-se num quadro e o roteiro dispara quando
    aquele quadro sai, expondo `return_value` NO MOMENTO DO DISPARO — que é
    quando o gdb de verdade sabe o valor, e não quando o breakpoint é armado."""

    armados = []

    def __init__(self, frame, internal=False):
        if frame.older() is None:
            raise error("quadro mais externo não tem endereço de retorno")
        # A identidade VERDADEIRA do quadro, do roteiro. O driver não vê isto: ele
        # tem de deduzir a dele lendo o quadro do CHAMADOR (ver _id_de_quadro), o
        # que é um caminho independente — se ele ler o registrador errado ou o
        # quadro errado, o teste falha.
        self.fid = frame._fid
        self.return_value = None
        self.silent = False
        FinishBreakpoint.armados.append(self)

    def stop(self):
        return True

    def out_of_scope(self):
        pass


def selected_frame():
    if _prog is None or _prog.terminou or _prog.i < 0 or _prog.i >= len(_prog.paradas):
        raise error("sem quadro")
    return _monta(_prog.paradas[_prog.i])


def _monta(quadros):
    """Constrói a cadeia de Frame de uma parada, do mais externo ao mais interno,
    e devolve o mais interno (que é o que o gdb chama de selected_frame).

    O `pc` de um quadro visto de baixo é o endereço de RETORNO da chamada que ele
    fez — uma propriedade da ARESTA (chamador, chamado), não do chamador. Duas
    chamadas na mesma linha são dois sítios de chamada, logo dois endereços; é
    exatamente isso que separa `fib(n-1)` de `fib(n-2)`, e modelar errado aqui
    tornaria o teste inútil."""
    construidos = []
    anterior = None
    for q in quadros:
        f = Frame(q["func"], q["line"], q.get("syms", []), q.get("file", _prog.arquivo),
                  q.get("decl", 0), q["sp"], 0, anterior)
        f._fid = (q["sp"], q.get("call_pc", 0))
        construidos.append((f, q))
        anterior = f
    # O pc exposto por um quadro é o call_pc do quadro que ele chamou.
    for i in range(len(construidos) - 1):
        construidos[i][0]._ret_pc = construidos[i + 1][1].get("call_pc", 0)
    return anterior


def _avanca():
    """Um `step`. Dispara os breakpoints cujos quadros sumiram da pilha nova."""
    if _prog.i + 1 >= len(_prog.paradas):
        _prog.terminou = True
        return False
    # O valor que cada quadro VIVO devolverá, colhido enquanto ele ainda está de
    # pé — o gdb lê o `return_value` no disparo, não na hora de armar.
    for q in _prog.paradas[_prog.i]:
        if q.get("retorna") is not None:
            _prog.retornos[(q["sp"], q.get("call_pc", 0))] = q["retorna"]
    _prog.i += 1
    texto = _prog.saidas.get(_prog.i)
    if texto and _saida_arquivo:
        with open(_saida_arquivo, "a") as fh:
            fh.write(texto)
    vivos = {(q["sp"], q.get("call_pc", 0)) for q in _prog.paradas[_prog.i]}
    for bp in list(FinishBreakpoint.armados):
        if bp.fid not in vivos and not getattr(bp, "_disparou", False):
            bp._disparou = True
            bp.return_value = _prog.retornos.get(bp.fid)
            bp.stop()
    return True


def execute(cmd, to_string=False):
    if cmd == "step":
        if not _avanca():
            raise error("programa terminou")
        return ""
    if cmd == "finish":
        if not _avanca():
            raise error("programa terminou")
        return ""
    if cmd.startswith("run"):
        _prog.i = 0
        texto = _prog.saidas.get(0)
        if texto and _saida_arquivo:
            with open(_saida_arquivo, "a") as fh:
                fh.write(texto)
        return ""
    if cmd == "continue":
        for j in range(_prog.i + 1, len(_prog.paradas)):
            texto = _prog.saidas.get(j)
            if texto and _saida_arquivo:
                with open(_saida_arquivo, "a") as fh:
                    fh.write(texto)
        _prog.terminou = True
        return ""
    if cmd.startswith("quit"):
        raise SystemExit(0)
    return ""


def instalar(programa, saida_arquivo=None):
    """Põe este módulo no lugar do `gdb` e arma o roteiro."""
    global _prog, _saida_arquivo
    _prog = programa
    _saida_arquivo = saida_arquivo
    FinishBreakpoint.armados = []
    sys.modules["gdb"] = sys.modules[__name__]
