# Odin support (B.1) — status and the on-target steps left

Odin (<https://odin-lang.org>) is added as a **Shape B (static-compiled)** language
per [`ADDING-A-LANGUAGE.md`](./ADDING-A-LANGUAGE.md).

## What landed (in code, tested)

- `odinSpec` + `NewOdin` in [`compiled.go`](../../internal/executor/compiled.go):
  `odin build {src} -file -out:{out}`, run the native artifact, full-rootfs
  denylist jail, `parseOdinVersion`.
- Registered in [`cmd/runner/main.go`](../../cmd/runner/main.go).
- Spec-level tests in [`odin_test.go`](../../internal/executor/odin_test.go)
  (templates, `-file`, run-is-artifact, posture, version parse). These pass in
  the unit suite **without** the toolchain, exactly like the other compiled specs
  — the real compile+run is a CI Docker-smoke concern.

Registering Odin is safe before the image has the toolchain: `resolveBin`
degrades to the bare name and `detectVersion` returns "" (empty version) without
blocking boot. A run request for `odin` simply fails to compile until the image
ships the toolchain.

## Fechado on-target (2026-08-03) — o que foi medido

O toolchain está no Dockerfile e a linguagem roda de verdade pelo `/run`. As três
"escolhas conservadoras" abaixo foram verificadas na imagem; **duas estavam
erradas** e o spec foi corrigido.

| Escolha | Estava | Ficou | Evidência on-target |
|---|---|---|---|
| `runFullRootfs` | `true` | `true` (confirmado) | `file` no artefato: `dynamically linked, interpreter /lib64/ld-linux-x86-64.so.2` |
| `capAddressSpace` | `true` | **`false`** | não é o programa, é o COMPILADOR: `odin build` embute LLVM e sob o cap do jail morre com `src/common_memory.cpp(323): Panic: Out of Virtual Memory` — TODA request virava `compile_error`. Mesmo motivo do `go build`. |
| `maxOpenFiles` | (não existia) | **`1024`** | o default do nsjail é 32 descritores; o compilador lê o `core/` arquivo a arquivo e falhava no meio, acusando um arquivo DIFERENTE a cada run (`core/io/util.odin(5:1) Syntax Error: Unknown error whilst reading file`) — parece instalação corrompida e não é |
| seccomp | denylist | denylist (confirmado) | o caminho do allowlist estático não se aplica: ele é para artefato estático em rootfs mínimo, e o binário do Odin é dinâmico. O denylist não bloqueia nada que o Odin precise (hello-world, `core:net`, alocação e loop rodaram) |

Duas descobertas que o rascunho abaixo não previa:

1. **`clang` é obrigatório.** O Odin dirige o LINK por clang. Sem ele o link falha
   e — pior — `odin` ainda **sai com código 0**, sem artefato, o que aparece como
   o `internal_error` "compile produced no artifact" em vez de um erro claro.
   `-linker:lld` não evita (também passa por clang) e o release não traz lld.
2. **O asset do release é `.tar.gz`, não `.zip`**, e o diretório de topo
   (`odin-linux-amd64-nightly+2026-07-10`) **não casa com a tag** (`dev-2026-07a`),
   então não dá para derivá-lo da versão — o Dockerfile usa `--strip-components=1`.
   E `vendor/` pode ser esvaziado, mas o DIRETÓRIO precisa existir, senão todo
   compile morre com `Internal Compiler Error: Cannot find the library collection`.

### Smoke on-target (rodado dentro da imagem, via `/run`)

| caso | resultado |
|---|---|
| `success` (`package main` + `main :: proc()`) | **OK** — stdout `42` |
| `compile_error` | **OK** — o diagnóstico real do compilador chega ao aluno |
| `timeout` | **OK** |
| egress contido (`core:net` para 1.1.1.1:53) | **OK** — `egress-blocked` |
| `memory_exceeded` | **OK** — validado na VPS, com cgroup delegado |

Rodado em duas condições, porque a diferença importa:

- **local, `RUNNER_CGROUP=auto` sem cgroup delegado**: o bomb sai como
  `runtime_error` (exit 137, morto pelo limite do CONTAINER, não por um teto
  por-run);
- **na VPS, `RUNNER_CGROUP=require` com o cgroup delegado do R6** (container
  isolado na 8091, removido depois): sai como **`memory_exceeded`**, classificado
  pelo evento OOM do kernel — o mesmo caminho que `deploy.sh verify` prova para
  go/js/java.

**Consequência a registrar:** com `capAddressSpace:false` o Odin passa a depender
do cgroup para o teto de memória, e o Odin morre com **stderr vazio**, então não
existe marcador possível para o fallback `memErrSubstr`. Em modo sem cgroup,
Odin não tem teto de memória por run — ele entra na mesma lista que
`deploy/README.md` já documenta para Go/JS/Java/TS.

## Rascunho original (mantido para histórico)

## What is NOT done here (needs a build+run environment)

The dev/unit image has no Odin toolchain and the release download is
proxy-blocked in this environment, so the two things the recipe defers to the
target are still open:

### 1. Dockerfile — install the toolchain

Odin ships prebuilt release tarballs that bundle their own LLVM. Add a stanza to
the **runtime** stage of [`Dockerfile`](../../Dockerfile) (pin the version and,
ideally, a sha256), roughly:

```dockerfile
# ---- Odin toolchain (B.1) for the `odin` compile jail ----
# Prebuilt release bundles LLVM; extract to /opt/odin and expose `odin` on PATH.
ARG ODIN_VERSION=dev-2024-05
RUN set -eux; \
    curl -fsSL -o /tmp/odin.zip \
      "https://github.com/odin-lang/Odin/releases/download/${ODIN_VERSION}/odin-linux-amd64-${ODIN_VERSION}.zip"; \
    mkdir -p /opt/odin && unzip -q /tmp/odin.zip -d /opt/odin && rm /tmp/odin.zip; \
    ln -s /opt/odin/odin /usr/local/bin/odin; \
    chmod -R a+rX /opt/odin
ENV PATH="/opt/odin/bin:${PATH}"
```

Verify the actual release asset name/layout for the pinned version (the archive's
top-level directory and whether `odin` sits at the root or under a subdir) before
trusting the paths above. If the CI build network also blocks the GitHub release,
vendor the tarball or build Odin from source (`make` against a pinned LLVM-dev).

### 2. Confirm the conservative posture choices

`odinSpec` documents three choices made without a local compile; validate each on
the target and adjust the spec:

| Choice | Set to | Verify | If wrong |
|---|---|---|---|
| `runFullRootfs` | `true` (dynamic libc) | `ldd` the artifact — dynamic? | If static-linkable (`-extra-linker-flags:"-static"`), flip to `false` for the minimal rootfs |
| `capAddressSpace` | `true` (normal allocator) | hello-world under `RLIMIT_AS` (256 MB) | If it aborts at startup, set `false` (cgroup-only, like Go/JVM) |
| seccomp | denylist | run representative programs with `RUNNER_STATIC_SECCOMP=complain`, read `dmesg` | widen the allowlist only if measured, else keep denylist |

### 3. On-target run tests (CI smoke)

Add, alongside the other compiled smoke cases: a `success` hello-world
(`package main` + `main :: proc()`), a `memory_exceeded` alloc bomb, a `timeout`
infinite loop, and egress-contained. The unit `odin_test.go` stays spec-level.

## Entrypoint convention

Single-file Odin needs `package main` and a `main :: proc()` in `main.odin`; the
`-file` flag builds that one file. Multi-file (Odin's package = a directory) maps
onto the runner's `files[]` model as a follow-up (mirror the compiled multi-file
build), not part of B.1.
