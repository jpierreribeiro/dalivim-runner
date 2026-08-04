package executor

import (
	"strings"
	"testing"
)

// TestOdinSpec pins the Odin runtime shape (B.1): a native single-file build via
// `odin build -file`, run as the artifact on the full-rootfs denylist jail. This
// is a SPEC-LEVEL test like the other compiled specs — the real jail compile+run
// is proven on-target by the CI Docker smoke, because the Odin toolchain is not
// present in the dev/unit-test image.
func TestOdinSpec(t *testing.T) {
	joined := strings.Join(odinSpec.compile, " ")
	if !strings.Contains(joined, "{src}") || !strings.Contains(joined, "{out}") {
		t.Fatalf("odin compile must template {src} and {out}: %v", odinSpec.compile)
	}
	// -file builds the single source rather than a package directory.
	if !strings.Contains(joined, "-file") {
		t.Fatalf("odin single-file build must pass -file: %v", odinSpec.compile)
	}
	if odinSpec.compile[0] != "odin" || odinSpec.compile[1] != "build" {
		t.Fatalf("odin compile must invoke `odin build`: %v", odinSpec.compile)
	}
	if strings.Join(odinSpec.run, " ") != "{out}" {
		t.Fatalf("odin run must be exactly the artifact: %v", odinSpec.run)
	}
	if odinSpec.sourceFile != "main.odin" {
		t.Fatalf("odin source file must be main.odin, got %q", odinSpec.sourceFile)
	}
	// A default odin build links libc dynamically → keep the full rootfs at run.
	if !odinSpec.runFullRootfs {
		t.Fatal("odin must keep the full rootfs (dynamic libc link) until a static build is verified")
	}
	// LLVM runtime syscall surface is wider than the C allowlist → denylist first.
	if odinSpec.staticAllowlistOK {
		t.Fatal("odin must stay on the denylist first (LLVM runtime syscalls)")
	}
	// Decided ON TARGET, and about the COMPILER rather than the program: the
	// artifact tolerates a hard RLIMIT_AS, but this flag gates both phases and
	// `odin build` embeds LLVM — under the compile jail's cap it panics with
	// "Out of Virtual Memory", which turned EVERY odin request into a
	// compile_error. Flipping this back re-breaks the language completely.
	if odinSpec.capAddressSpace {
		t.Fatal("odin must NOT cap address space: the LLVM-backed compiler panics under RLIMIT_AS (like `go build`)")
	}
	if len(odinSpec.binNames) == 0 || odinSpec.binNames[0] != "odin" {
		t.Fatalf("odin binNames must resolve `odin`: %v", odinSpec.binNames)
	}
	if odinSpec.parseVersion == nil {
		t.Fatal("odin must supply a version parser")
	}
}

func TestParseOdinVersion(t *testing.T) {
	cases := map[string]string{
		"odin version dev-2024-05:abc123": "dev-2024-05:abc123",
		"odin version 0.13.0":             "0.13.0",
		"dev-2024-05":                     "dev-2024-05", // no "version" word → last field
		"":                                "",
	}
	for in, want := range cases {
		if got := parseOdinVersion(in); got != want {
			t.Fatalf("parseOdinVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestOdinTraceRecipe pins the B.2 step-through recipe. Everything here is a
// security or correctness property that is invisible in a unit test run (there
// is no gdb, no jail) but decides what happens on target, so it is pinned by
// shape.
func TestOdinTraceRecipe(t *testing.T) {
	tc, ok := traceCommands["odin"]
	if !ok {
		t.Fatal("odin must have a trace recipe (B.2)")
	}
	if !tc.isCompiledTrace() {
		t.Fatal("odin's trace is the COMPILED shape: a tracer over an artifact, not an interpreter over source")
	}
	if len(tc.tracerBin) == 0 || tc.tracerBin[0] != "gdb" {
		t.Fatalf("odin must trace under gdb, got %v", tc.tracerBin)
	}
	// -debug is the whole precondition: no DWARF, no line table, no locals — the
	// tutor would step a binary it cannot read.
	if strings.Join(tc.compileExtra, " ") != "-debug" {
		t.Fatalf("odin's trace build must force -debug, got %v", tc.compileExtra)
	}
	// -nx: a submission must never get to script the debugger through a .gdbinit
	// it dropped in the workdir.
	joined := strings.Join(tc.argvTail, " ")
	if !strings.Contains(joined, "-nx") {
		t.Fatalf("gdb must run with -nx so a workdir .gdbinit cannot script it: %v", tc.argvTail)
	}
	if !strings.Contains(joined, "--batch") {
		t.Fatalf("gdb must run in batch mode (never an interactive prompt): %v", tc.argvTail)
	}
	// The student breaks on THEIR main, not the Odin runtime's C entrypoint.
	if tc.entrySymbol != "main::main" {
		t.Fatalf("odin's entry symbol must be main::main, got %q", tc.entrySymbol)
	}
	// The driver filename must not be shadowable by a submission's own file.
	if !strings.HasPrefix(tc.harnessName, "_dalivim_") {
		t.Fatalf("the driver filename must be runner-fixed and unshadowable, got %q", tc.harnessName)
	}
	if tc.traceFormat != "dalivim-trace-json@2" {
		t.Fatalf("odin emits the v2 object-graph format, got %q", tc.traceFormat)
	}
	env := strings.Join(tc.env("main.odin", "", "trace.json", 100, 1000), " ")
	for _, want := range []string{"DALIVIM_TRACE_TARGET=main.odin", "DALIVIM_TRACE_ENTRY=main::main", "DALIVIM_TRACE_MAX_STEPS=100"} {
		if !strings.Contains(env, want) {
			t.Fatalf("trace env must carry %q: %s", want, env)
		}
	}
}

// TestOdinTraceDriver_HidesUndefinedVariables is the pedagogy guard. A native
// local exists (in DWARF) for the whole frame but holds stack GARBAGE until its
// initialising line runs — measured on target as n = 140737488344096 before
// `n := 3`. Showing that teaches the student something false, so the driver
// compares the current line against the declaring line. This test pins that the
// rule is STRICTLY greater and that arguments are gated on the function's line.
func TestOdinTraceDriver_HidesUndefinedVariables(t *testing.T) {
	if !strings.Contains(traceDriverGDB, "current_line <= decl") {
		t.Fatal("the driver must hide a local until execution has PASSED its declaring line")
	}
	if !strings.Contains(traceDriverGDB, "current_line <= func_line") {
		t.Fatal("the driver must hide arguments until the prologue is past the function's line")
	}
	// A pointer is never dereferenced: a dangling one is how a hostile program
	// would crash the tracer.
	if !strings.Contains(traceDriverGDB, "TYPE_CODE_PTR") || !strings.Contains(traceDriverGDB, "0x%x") {
		t.Fatal("the driver must report a pointer as an address, never dereference it")
	}
	if !strings.Contains(traceDriverGDB, "IMPLICIT_NAMES") {
		t.Fatal("the driver must drop compiler-injected names (Odin's `context`)")
	}
}

// TestOdinTraceDriver_ContainersShowValues pins the rule that makes the Odin
// canvas teach anything: Odin's composite types are {data,len} STRUCTS in DWARF,
// so the generic struct path renders them faithfully — and uselessly. A student
// saw `{data: 0x5555…, len: 3}` where the value is `"ola"` or `[10, 20, 30]`.
// The driver must recognise them by NAME, before the struct path.
func TestOdinTraceDriver_ContainersShowValues(t *testing.T) {
	for _, want := range []string{"_odin_string", "_odin_sequence", "_odin_map"} {
		if !strings.Contains(traceDriverGDB, want) {
			t.Fatalf("the driver must render Odin containers by value (%s missing)", want)
		}
	}
	// Recognised BEFORE the generic struct expansion, or the {data,len} shape wins.
	iName := strings.Index(traceDriverGDB, `tname in ("string", "cstring")`)
	iStruct := strings.Index(traceDriverGDB, `key = ("s", str(v.address)`)
	if iName < 0 || iStruct < 0 || iName > iStruct {
		t.Fatal("container recognition must come BEFORE the generic struct path")
	}
	// A length read out of a hostile program drives the read loop, so it is
	// itself validated — an unchecked `len` is an arbitrary-size memory read.
	if !strings.Contains(traceDriverGDB, "MAX_SANE_LEN") || !strings.Contains(traceDriverGDB, "def _sane_len") {
		t.Fatal("the driver must sanity-check a container's len before walking it")
	}
	// The map's internals are runtime-private; inventing entries would be a lie
	// that breaks on the next toolchain bump.
	if !strings.Contains(traceDriverGDB, "internal and version-specific") {
		t.Fatal("the map box must stay honest (type + length), not fake its entries")
	}
}

// TestOdinTraceDriver_CallAndReturnEvents: o gdb entrega só "parei numa linha".
// Sem sintetizar `call`/`return` o passo a passo do Odin era uma lista plana de
// linhas — 20 eventos `line` e ZERO `return` no mesmo programa em que o Python
// produz 5 e 5 —, então o aluno nunca via entrar numa proc nem sair dela.
func TestOdinTraceDriver_CallAndReturnEvents(t *testing.T) {
	for _, want := range []string{`snap["event"] = "call"`, `steps[-1]["event"] = "return"`} {
		if !strings.Contains(traceDriverGDB, want) {
			t.Fatalf("the driver must synthesise call/return events (%s missing)", want)
		}
	}
	// O `return` mora no passo ANTERIOR: é lá que o quadro ainda está de pé, que
	// é a semântica do evento de retorno do Python.
	if !strings.Contains(traceDriverGDB, "len(pilha) < len(pilha_ant)") {
		t.Fatal("a return must be detected by the stack getting SHALLOWER")
	}
}

// TestOdinTraceDriver_CapturesReturnValue: a linha `devolveu` — o "Return value"
// do Python Tutor — é o que fecha o ciclo mental de uma chamada. Sem ela o aluno
// vê a proc terminar e nunca vê o que ela entregou.
func TestOdinTraceDriver_CapturesReturnValue(t *testing.T) {
	if !strings.Contains(traceDriverGDB, "gdb.FinishBreakpoint") {
		t.Fatal("the return value must come from gdb's FinishBreakpoint, not a hand-read register")
	}
	// stop() devolve False: registrar o valor NUNCA pode parar o programa do aluno.
	i := strings.Index(traceDriverGDB, "def stop(self)")
	if i < 0 || !strings.Contains(traceDriverGDB[i:i+400], "return False") {
		t.Fatal("the finish breakpoint must record the value WITHOUT stopping the inferior")
	}
	if !strings.Contains(traceDriverGDB, `passo["retval"]`) {
		t.Fatal("the captured value must be attached to the return step as retval")
	}
}

// TestOdinTraceDriver_StableObjectIds: o mapa de ids era refeito a cada passo, e
// a identidade de uma caixa não atravessava um passo — a propriedade que o
// diagrama existe para ensinar. É o mesmo conserto do harness Python.
func TestOdinTraceDriver_StableObjectIds(t *testing.T) {
	if !strings.Contains(traceDriverGDB, "class Registro") {
		t.Fatal("ids must live in a per-TRACE registry, not a per-step map")
	}
	if !strings.Contains(traceDriverGDB, "registro.podar(") {
		t.Fatal("the registry must be pruned to each step's live set (address reuse)")
	}
	// O teto de objetos continua sendo POR PASSO; um registro de trace inteiro
	// não pode servir de desculpa para um heap ilimitado num passo.
	if !strings.Contains(traceDriverGDB, "len(self.usados) >= MAX_HEAP_OBJECTS") {
		t.Fatal("the per-STEP heap cap must survive the shared registry")
	}
}

// TestOdinTraceDriver_EmptyContainersAreDistinct: com o ponteiro de dados NULO
// como chave, dois `[dynamic]int` vazios distintos caíam na MESMA caixa — o
// diagrama desenhava duas setas para um objeto só e ensinava que `a` e `b` são
// aliases quando não são.
func TestOdinTraceDriver_EmptyContainersAreDistinct(t *testing.T) {
	if !strings.Contains(traceDriverGDB, "def _chave") || !strings.Contains(traceDriverGDB, "vazio@") {
		t.Fatal("a null data pointer must fall back to the value's own address")
	}
	for _, marca := range []string{`_chave("seq"`, `_chave("map"`} {
		if !strings.Contains(traceDriverGDB, marca) {
			t.Fatalf("%s must go through the null-safe key", marca)
		}
	}
}

// TestOdinTraceDriver_ExitStatusMatchesRunMode: o gdb sai sempre 0, então um
// programa que estourava era classificado como SUCESSO em modo trace enquanto o
// mesmo programa dava runtime_error em modo run (medido: `xs[10]` numa fatia de
// 3). O aluno via "execução concluída" para um programa que quebrou.
func TestOdinTraceDriver_ExitStatusMatchesRunMode(t *testing.T) {
	if !strings.Contains(traceDriverGDB, `gdb.execute("quit %d"`) {
		t.Fatal("the driver must exit with the STUDENT program's status, not gdb's")
	}
	if !strings.Contains(traceDriverGDB, "gdb.events.exited.connect") ||
		!strings.Contains(traceDriverGDB, "gdb.events.stop.connect") {
		t.Fatal("how the inferior ended is only knowable through gdb's events")
	}
	// E a quebra vira registro: sem ele o passo a passo apenas para, e a pergunta
	// "onde quebrou?" fica sem resposta.
	if !strings.Contains(traceDriverGDB, `ultimo["event"] = "exception"`) {
		t.Fatal("a fatal signal must mark the last step as the crash")
	}
}

// TestOdinTraceDriver_CrashSaysWhatBroke: o sinal responde "como", não "o quê".
// Dizer `SIGILL` a um aluno é o mesmo que o Python dizer `SIGFPE` em vez de
// `ZeroDivisionError` — e o runtime do Odin já escreveu a explicação boa no
// stderr ("Index 10 is out of range 0..<3"). É ela que o "Quebrou aqui" mostra.
func TestOdinTraceDriver_CrashSaysWhatBroke(t *testing.T) {
	if !strings.Contains(traceDriverGDB, "def _panico_do_odin") {
		t.Fatal("the crash message must prefer the Odin runtime's own sentence")
	}
	// O stderr é escrito pelo programa do ALUNO: leitura limitada, nunca inteira.
	if !strings.Contains(traceDriverGDB, "MAX_STDERR_SCAN") ||
		!strings.Contains(traceDriverGDB, "fh.read(MAX_STDERR_SCAN)") {
		t.Fatal("the student's stderr is hostile input and must be read under a cap")
	}
	// E sem a frase (um SIGSEGV cru) ainda há de sobrar o nome do sinal.
	if !strings.Contains(traceDriverGDB, `else _fim["sinal"]`) {
		t.Fatal("a crash with no parseable sentence must still fall back to the signal")
	}
}
