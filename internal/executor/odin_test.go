package executor

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestOdinTraceDriver_Behaviour EXECUTA o driver, em vez de afirmar sobre o texto
// dele como todo o resto deste arquivo faz.
//
// A diferença importa: um teste de texto passa em qualquer refactor que preserve
// as strings e falha em qualquer renomeação que não mude nada, e foi assim que o
// driver do Odin chegou a produção sem uma única garantia de COMPORTAMENTO — ao
// contrário do harness Python, cujos testes rodam CPython de verdade e foi um
// deles que pegou o bug de identidade de caixa.
//
// A imagem de dev não tem gdb nem o toolchain do Odin, então o driver roda contra
// um `gdb` falso (trace_driver_gdb_fake.py) sobre um programa roteirizado. Isso
// NÃO substitui o smoke on-target — nada aqui prova coisa alguma sobre o DWARF do
// Odin —, mas prova toda a lógica que mora no driver: identidade de caixa, delta
// do heap, atribuição do valor devolvido, contagem de saída e os tetos.
func TestOdinTraceDriver_Behaviour(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	script, err := filepath.Abs("trace_driver_gdb_test.py")
	if err != nil {
		t.Fatalf("resolve script: %v", err)
	}
	out, err := exec.Command("python3", script).CombinedOutput()
	if err != nil {
		t.Fatalf("driver behaviour checks failed:\n%s", out)
	}
	if !strings.Contains(string(out), "tudo verde") {
		t.Fatalf("expected every check to pass:\n%s", out)
	}
}

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
	// Um ponteiro SEM forma declarada nunca é desreferenciado — `rawptr`, ponteiro
	// para função, ponteiro para escalar. Só `^T` para agregado é seguido, e é o
	// que faz uma lista ligada virar caixas e setas (ver _aponta_para_agregado).
	if !strings.Contains(traceDriverGDB, "TYPE_CODE_PTR") || !strings.Contains(traceDriverGDB, "0x%x") {
		t.Fatal("a pointer with no drawable target must be reported as its address")
	}
	if !strings.Contains(traceDriverGDB, "def _aponta_para_agregado") {
		t.Fatal("only a pointer to an aggregate may be followed")
	}
	i := strings.Index(traceDriverGDB, "def _aponta_para_agregado")
	for _, code := range []string{"TYPE_CODE_STRUCT", "TYPE_CODE_ARRAY", "TYPE_CODE_UNION"} {
		if !strings.Contains(traceDriverGDB[i:i+700], code) {
			t.Fatalf("the aggregate filter must name %s", code)
		}
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
	iStruct := strings.Index(traceDriverGDB, `key = ("s", _addr_int(v)`)
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
	//
	// E o sinal NÃO é a pilha ter encolhido — é o breakpoint de saída ter
	// disparado. `f(n-1) + f(n-2)` põe duas chamadas na mesma linha: a primeira
	// sai e a segunda entra sem passo no nível do chamador, a profundidade fica
	// igual, e o retorno seria invisível.
	if !strings.Contains(traceDriverGDB, "d[1].saiu") {
		t.Fatal("a return must be detected by the finish breakpoint FIRING")
	}
	if !strings.Contains(traceDriverGDB, "self.saiu = True") {
		t.Fatal("the finish breakpoint must record that its frame left")
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
	// E SÓ quando dá para provar de quem o valor é. Em recursão não dá: duas
	// chamadas na mesma linha (`f(n-1) + f(n-2)`) não deixam passo no nível do
	// chamador, e nem a profundidade nem a ordem de disparo identificam o quadro.
	// Medido em fibonacci(6): um quadro com n=0 dizendo `devolveu 8`. Um valor
	// errado é pior que valor nenhum — ele ensina que fib(0) devolve 8.
	if !strings.Contains(traceDriverGDB, "len(saidos) == 1 and not repetida") {
		t.Fatal("retval must be omitted when the returning frame is ambiguous (recursion)")
	}
	// A QUARTA abordagem: identificar o quadro pelo ENDEREÇO dele, e não pela
	// posição. As três anteriores responderam por profundidade ou por ordem de
	// disparo — nenhuma das duas distingue `f(n-1)` de `f(n-2)`, que moram na
	// mesma linha. O par (pc do chamador, sp do chamador) distingue: dois sítios
	// de chamada são dois endereços de retorno.
	if !strings.Contains(traceDriverGDB, "def _id_de_quadro") {
		t.Fatal("the returning frame must be identified by its ADDRESS, not by depth or firing order")
	}
	iChave := strings.Index(traceDriverGDB, "def _id_de_quadro")
	for _, want := range []string{"pai.pc()", `pai.read_register("sp")`} {
		if !strings.Contains(traceDriverGDB[iChave:iChave+1600], want) {
			t.Fatalf("the frame key must come from the CALLER's frame (%s missing)", want)
		}
	}
	// E a regra antiga fica como PISO: a chave nova ainda não foi medida
	// on-target, então ela só pode ACRESCENTAR valor onde ele é provável, nunca
	// sobrepor um silêncio com um palpite.
	if !strings.Contains(traceDriverGDB, "if dono is None and len(saidos) == 1 and not repetida:") {
		t.Fatal("the conservative rule must remain the FLOOR when the frame key does not resolve")
	}
}

// TestOdinTraceDriver_ByteBudgetIsPerStep: o teto de bytes media
// `len(json.dumps(steps))` — a lista INTEIRA acumulada, uma vez por passo —, o
// que faz o custo do próprio medidor crescer com o quadrado do número de passos.
// Medido: 2,0 s em 533 passos (o `fib(10)`, ~15% dos 13 s) e 46,7 s em 2500, num
// envelope de 15 s. O teto de 2500 passos era INALCANÇÁVEL: um trace longo morria
// de timeout dentro do medidor e o aluno recebia um erro no lugar de um trace
// parcial. O harness Python sempre mediu por passo.
func TestOdinTraceDriver_ByteBudgetIsPerStep(t *testing.T) {
	if strings.Contains(traceDriverGDB, "len(json.dumps(steps))") {
		t.Fatal("the byte cap must not re-serialise the whole accumulated list on every step")
	}
	if !strings.Contains(traceDriverGDB, "custo = len(json.dumps(snap))") {
		t.Fatal("the byte cap must measure ONE step and accumulate the number")
	}
}

// TestOdinTraceDriver_HeapTravelsAsDelta: o Odin mandava o heap CHEIO em todo
// passo — o item 3 da lista de pendências. Mesma codificação do harness Python,
// e o mesmo consumidor remonta os dois.
func TestOdinTraceDriver_HeapTravelsAsDelta(t *testing.T) {
	if !strings.Contains(traceDriverGDB, `"heap_encoding": "delta"`) {
		t.Fatal("the Odin trace must announce the delta encoding like the Python harness")
	}
	if !strings.Contains(traceDriverGDB, `snap["heap_delta"] = delta`) {
		t.Fatal("steps after the first must carry only what changed")
	}
	// A base do delta só avança DEPOIS de o passo entrar: um passo descartado pelo
	// teto deixaria o delta seguinte apontando para um heap que nunca foi enviado.
	iAppend := strings.Index(traceDriverGDB, "steps.append(snap)")
	iBase := strings.Index(traceDriverGDB, "heap_ant = cheio")
	if iAppend < 0 || iBase < 0 || iBase < iAppend {
		t.Fatal("the delta baseline must only advance after the step is actually kept")
	}
}

// TestOdinTraceDriver_LimitsAreVisible: o teto de caixas por passo era escrito em
// três lugares e NUNCA serializado — campo morto. Para o aluno isso era a
// variável virando um ponto que não aponta para lugar nenhum (o frontend não
// desenha seta para caixa que não existe), sem nenhuma explicação.
func TestOdinTraceDriver_LimitsAreVisible(t *testing.T) {
	if !strings.Contains(traceDriverGDB, `passo["heap_truncated"] = True`) {
		t.Fatal("hitting the per-step box cap must be recorded on the step")
	}
	if !strings.Contains(traceDriverGDB, `"heap": heap_truncated,`) {
		t.Fatal("the document's truncated{} must carry the heap cap, or it never reaches the student")
	}
}

// TestOdinTraceDriver_StdoutLenIsReal: o driver mandava `stdout_len: 0` em TODO
// passo, então o painel "Saída até aqui" dizia "(sem saída ainda)" em todos os
// passos de todo programa Odin — inclusive no último, de um programa que
// imprimiu. O contrato conta CODE POINTS (o consumidor fatia com Array.from).
func TestOdinTraceDriver_StdoutLenIsReal(t *testing.T) {
	if strings.Contains(traceDriverGDB, `"stdout_len": 0,`) {
		t.Fatal("stdout_len must not be a hardcoded zero")
	}
	if !strings.Contains(traceDriverGDB, "def _stdout_ate_agora") {
		t.Fatal("the driver must count the inferior's own output")
	}
	i := strings.Index(traceDriverGDB, "def _stdout_ate_agora")
	// Incremental: reler o arquivo inteiro a cada passo seria o mesmo erro
	// quadrático que o teto de bytes tinha.
	if !strings.Contains(traceDriverGDB[i:i+2200], `fh.seek(_saida["bytes"])`) {
		t.Fatal("the counter must read only the NEW bytes, not the whole file each step")
	}
	if !strings.Contains(traceDriverGDB[i:i+2200], "getincrementaldecoder") {
		t.Fatal("code points, not bytes: a multibyte char split across two reads must not become two")
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

// TestOdinTraceDriver_PointersBecomeArrows: um `prox: ^Node` saía como
// `0x7fffffffe9b8` — o aluno via um número onde o Python Tutor desenha uma seta,
// e uma estrutura ligada não se desenhava. As duas pontas tinham a informação e
// nunca casavam: a caixa era chaveada por `str(v.address)` (que o gdb imprime
// como `(main::Node *) 0x7fff…`) e o ponteiro saía como `0x%x`.
func TestOdinTraceDriver_PointersBecomeArrows(t *testing.T) {
	if !strings.Contains(traceDriverGDB, "def _addr_int") {
		t.Fatal("both ends must speak the same address format")
	}
	if !strings.Contains(traceDriverGDB, "def _liga_ponteiros") ||
		!strings.Contains(traceDriverGDB, "por_endereco") {
		t.Fatal("a pointer into a box already on screen must become a ref")
	}
	// A ligação é uma CONSULTA, não uma leitura: a regra de nunca desreferenciar
	// um ponteiro cru continua valendo, e o teste dela segue neste arquivo.
	i := strings.Index(traceDriverGDB, "def _liga_ponteiros")
	if i < 0 || !strings.Contains(traceDriverGDB[i:i+1200], "NÃO desreferencia") {
		t.Fatal("the linking pass must state that it never dereferences")
	}
	// Precisa rodar DEPOIS do heap estar completo: quando `a.prox` é lido, a
	// caixa de `b` pode ainda não existir.
	iDrain := strings.Index(traceDriverGDB, "_drain_heap(idmap, heap, queue)")
	iLink := strings.Index(traceDriverGDB, "_liga_ponteiros(stack,")
	if iDrain >= 0 && iLink >= 0 && iLink < iDrain {
		t.Fatal("pointer linking must run after every box for the step exists")
	}
	// `nil` é o que o aluno escreveu; `0x0` é o que a máquina guardou.
	if !strings.Contains(traceDriverGDB, `_prim("ptr", "nil")`) {
		t.Fatal("a null pointer must read as nil, not 0x0")
	}
}

// TestOdinTraceDriver_LinkedStructureExpands: uma lista ligada com `new()` é o
// exercício em que o diagrama mais ensina, e era exatamente o que não se
// desenhava — nenhuma variável alcança os nós além do ponteiro, então não havia
// caixa nenhuma, só endereços.
func TestOdinTraceDriver_LinkedStructureExpands(t *testing.T) {
	if !strings.Contains(traceDriverGDB, "def _expande_ponteiros") {
		t.Fatal("a pointer's target must get a box, or a linked list draws nothing")
	}
	// EM LARGURA, com fila: recursão sob MAX_DEPTH cortaria a lista no 4º nó.
	// O que limita é o teto de caixas de sempre.
	i := strings.Index(traceDriverGDB, "def _expande_ponteiros")
	trecho := traceDriverGDB[i : i+2600]
	if !strings.Contains(trecho, "heap.pendentes.pop(0)") {
		t.Fatal("expansion must be breadth-first (a queue), not depth-limited recursion")
	}
	if !strings.Contains(trecho, "len(heap.usados) < MAX_HEAP_OBJECTS") {
		t.Fatal("expansion must be bounded by the same per-step box cap")
	}
	// DOIS tetos, e o segundo é o que impede uma regressão de tempo: o custo é
	// passos × caixas, então um teto só por passo ainda deixa um laço que
	// constrói 150 nós virar timeout — entregar NADA ao aluno, onde antes ele
	// tinha um trace sem desenho. Medido: 13s com orçamento de 2000, 7,9s com 600.
	if !strings.Contains(trecho, "expandidos < MAX_EXPANDIDOS_POR_PASSO") {
		t.Fatal("expansion must be capped per step")
	}
	if !strings.Contains(trecho, `_orcamento["resta"] > 0`) {
		t.Fatal("expansion must also have a budget for the whole trace")
	}
	// A relaxação da regra tem de estar declarada onde ela acontece, com o custo.
	if !strings.Contains(trecho, "free") {
		t.Fatal("the trade-off (reading after a free) must be stated at the site")
	}
	// Um alvo pendurado degrada para o endereço, nunca derruba o driver.
	if !strings.Contains(trecho, "except Exception:") {
		t.Fatal("a dangling target must degrade, not crash the tracer")
	}
}
