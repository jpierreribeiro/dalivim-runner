# B.2 — spike: passo a passo para linguagem compilada (Odin)

Status: **spike de pesquisa concluído. NÃO é feature, não tem data.** O portão
que o [estudo](./../TUTOR-EVOLUTION-STUDY.md) definiu era a revisão de segurança
do tracer dentro do jail; ela foi feita e está abaixo, com medições em vez de
suposições. A recomendação final é **não graduar ainda** — por um motivo que não
é o esperado (não é o sandbox).

Tudo aqui foi medido em máquina real (kernel 6.8, Odin `dev-2026-07-nightly`,
gdb 15.0.50). Os artefatos do spike estão em `docs/future/b2-spike/`.

> Reprodutibilidade: a máquina do spike tinha `ODIN_ROOT` apontando para uma
> instalação própria do Odin — mesma versão (`dev-2026-07-nightly:819fdc7`) da
> que o Dockerfile pina, então o DWARF e o comportamento do gdb valem. Ao repetir
> isto na imagem, confira `ODIN_ROOT`: ele silenciosamente decide qual árvore
> `core/` é usada, e foi o que mascarou um erro de empacotamento durante a B.1.

---

## 1. A menor fatia end-to-end: funciona

Um `prog.odin` trivial compilado com `-debug`, percorrido por um driver
`gdb --batch` + gdb-python, emitindo **trace v2 (`dalivim-trace-json@2`)** — o
mesmo formato que o harness Python do A0 produz:

```
step 0 linha 16  main::main   {'n': '140737488344096', 'm': '93824992510853', 'p': 'ref->1', ...}
step 1 linha 17  main::main   {'n': '3', 'm': '93824992510853', 'p': 'ref->1', ...}
step 2 linha 18  main::main   {'n': '3', 'm': '4', 'p': 'ref->1', ...}
step 4 linha 10  main::soma   {'a': '140737488343944', 'b': '140737351058785'}
step 5 linha 11  main::soma   {'c': '0', 'a': '3', 'b': '4'}

heap: {"1": {"kind":"object","cls":"struct main::Ponto",
             "fields":{"x":{"prim":"int","text":"3"},"y":{"prim":"int","text":"4"}}}}
```

O `struct Ponto` virou uma caixa `kind:"object"` com `ref` — **o mesmo modelo do
A0**, então o ObjectCanvas do A1 desenharia isso sem uma linha de código novo.
Isso valida a tese da Parte C do estudo: o formato é neutro de linguagem.

O DWARF do Odin é bom: gdb resolve `main::main`/`main::soma`, arquivo, linha e
tipos de struct sem nenhum trabalho extra.

## 2. Revisão de segurança do tracer no jail (o portão)

O denylist compartilhado mata `ptrace, process_vm_readv, process_vm_writev`
(`internal/sandbox/nsjail.go`) — "tracer attach". Abrir isso para o tutor abre
para o **código do aluno** também, porque a política é uma só por jail.

### 2.1 A hipótese que eu ia escrever estava ERRADA

A objeção clássica é: "com ptrace o aluno traça um filho e, na parada de entrada
de syscall, reescreve o número da syscall **depois** do filtro seccomp — logo
seccomp deixa de ser fronteira". Testei em vez de afirmar
(`b2-spike/seccomp_ptrace_test.c`): filtro DEFAULT ALLOW + KILL em `mount`, filho
com `PTRACE_TRACEME` chamando `getppid`, pai reescrevendo `orig_rax` 110 → 165.

```
parada em syscall permitida nr=110 -> reescrevendo para a PROIBIDA nr=165
filho morto por sinal 31 (Bad system call)
RESULTADO: CONTIDO — o filtro pegou a syscall reescrita
```

**O kernel refiltra a syscall reescrita.** O bypass não existe neste kernel. Essa
era a objeção mais forte contra ptrace-no-jail e ela cai.

### 2.2 O que ptrace realmente concede aqui

Com o jail como está — uid privado, **PID namespace novo**, rootfs read-only,
`no_new_privs`, sem binário setuid, netns vazio — um tracer só alcança processos
do próprio jail, todos do mesmo uid e sem privilégio a escalar. O ganho de poder
é pequeno; o risco residual é o de sempre com ptrace (manipular memória/registro
de um processo irmão), e nesse escopo não há vítima interessante.

### 2.3 O bloqueio real é `/proc`, não ptrace

O jail passa `--disable_proc`. Medido:

| condição | resultado |
|---|---|
| gdb sem `/proc` | **quebra** — o binário é PIE e sem `/proc/<pid>/maps` o gdb não descobre o offset de carga; o breakpoint em `main::main` cai no loader dinâmico e sai "No line number information available" |
| gdb com procfs **fresco** montado dentro do PID namespace do jail | **funciona** — breakpoint certo, linha certa, locais lidos; e `ls /proc` mostra **4 PIDs**, só os do jail |

Ou seja: um perfil "trace" precisa de `ptrace` **e** de um procfs próprio. A
exposição é bem menor do que "religar /proc" sugere — é o /proc do PID namespace
do jail, não o do host — mas é uma mudança deliberada de postura, e deve ser um
**perfil separado, usado só em `mode:"trace"`**, nunca o default.

### 2.4 Valgrind muda a conta (e talvez a decisão)

| | gdb | Valgrind |
|---|---|---|
| pacotes apt (bookworm, `--no-install-recommends`) | **42** | **2** |
| precisa de `ptrace` no jail | **sim** | **não** (JIT no próprio processo) |
| precisa de `/proc` do inferior | **sim** (PIE) | do próprio processo |
| custo de implementação | baixo (gdb-python, provado acima) | **alto** — Python Tutor escreveu uma *tool* Valgrind sob medida; teríamos de portar para os layouts do Odin |
| custo de execução | baixo | 10–50× (emulador de CPU) |

O estudo previu que a revisão de segurança poderia decidir gdb-vs-Valgrind, e ela
de fato empurra para o Valgrind: **zero exceção de ptrace** e 2 pacotes contra 42.
O preço é inverter o custo — implementação barata/execução barata (gdb) vira
implementação cara/execução cara (Valgrind).

## 3. O motivo real para NÃO graduar ainda: semântica, não contenção

O spike expôs um problema que nenhuma das duas abordagens resolve de graça e que
o estudo não listou:

**Variável nativa não tem "ainda não existe".** Em Python, um nome não ligado
simplesmente não está no frame. Em Odin, o escopo DWARF cobre o frame inteiro, e
antes da linha que inicializa a variável ela contém **lixo da pilha**:

```
step 0 linha 16  n = 140737488344096   <- antes de `n := 3` rodar
step 1 linha 17  n = 3                  <- correto
step 4 linha 10  a = 140737488343944    <- entrada de soma(), antes do prólogo
```

Mostrar isso a um aluno é pior do que não mostrar nada: ensina que a variável
"tinha" um número gigante. Um tutor honesto precisa suprimir a variável até a
primeira escrita (aproximável por `decl_line <= linha atual`, refinável com
watchpoints). Isso é trabalho de **pedagogia**, não de leitura de DWARF, e é o
mesmo em gdb ou Valgrind.

Outros dois, menores, já resolvidos no protótipo:

- **`context`**: o Odin injeta essa struct implícita (ponteiros de procedure,
  allocator, logger) em todo frame. Ruído; filtrado por nome.
- **`step` desce na stdlib**: `fmt.println` → `fmt.fprintln` → `__memset_avx2` da
  libc. O driver só conta passo em arquivo do aluno e usa `finish` no resto.
- **valores hostis**: em frames da stdlib o gdb devolveu strings lidas de memória
  não inicializada (`"LC_IDENTIFICATION=pt_BR.UTF-8"` como conteúdo de uma
  variável) e levantou `MemoryError`. O protótipo trata toda leitura
  defensivamente e nunca desreferencia ponteiro — vira `opaque`. O requisito de
  hostilidade do estudo é atendível, e é obrigatório.

## 4. Recomendação

1. **Não graduar para feature agora.** A contenção — que era o poste alto — é
   tratável e está medida. O que falta é a camada de fidelidade pedagógica (§3),
   que é onde mora o risco de ensinar coisa errada.
2. **Se graduar, decidir gdb vs Valgrind com §2.4 na mão**, não pelo conforto de
   implementação. gdb entrega mais rápido; Valgrind evita abrir ptrace.
3. **Pré-requisito de qualquer versão real**: perfil de jail "trace" separado
   (ptrace + procfs próprio) usado só em `mode:"trace"`, com o resto do denylist
   intacto — nunca afrouxar o perfil padrão.
4. **Forçar `-debug`** (o runner controla a linha de compilação, então isso é de
   graça) e manter programas de um thread só, como o estudo já pedia.

Sem data, como combinado.
