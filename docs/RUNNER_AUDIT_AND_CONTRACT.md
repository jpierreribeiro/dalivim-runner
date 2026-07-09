# Runner Audit, API Contract, e Roadmap de Endurecimento

> **Contexto:** auditoria da implementação atual (`dalivim-runner`) contra o blueprint
> em [`RUNNER_ARCHITECTURE_AND_HARDENING_PLAN.md`](RUNNER_ARCHITECTURE_AND_HARDENING_PLAN.md).
> Este documento é a fonte de verdade para **riscos residuais**, **especificação do
> contrato de API** (máquina de estados de erro, fallback do Gateway), e o **plano
> técnico de integração do nsjail sem quebrar a interface do executor**.
>
> **Data:** 2026-07-09  
> **Status:** proposta técnica (implementação F-A/F-B segue este plano).

---

## Parte 1 — Auditoria e Riscos Residuais

### 1.1 Camada-a-camada: conformidade vs. o plano (§2.3)

| Camada (§2.3) | Alvo no plano | Estado atual | Veredito |
|---|---|---|---|
| **Rede** | netns vazio (CLONE_NEWUSER\|CLONE_NEWNET) | ✅ Implementado; probe no boot; egress → ENETUNREACH | **Conforme (F-03)** |
| **Syscalls** | seccomp kafel (denylist) | ❌ Ausente | **Lacuna → F-B** |
| **PIDs (fork bomb)** | `--rlimit_nproc` por-jail + bound concorrência | ⚠️ `RLIMIT_NPROC` global (bad—D-3 rejeita) | **Não-conforme + nocivo** |
| **Memória** | `--rlimit_as` | ✅ `ulimit -v` em python.go:78 | **Conforme** |
| **CPU** | `--rlimit_cpu` + wall | ✅ `ulimit -t` + context.WithTimeout + SIGKILL-pgroup | **Conforme** |
| **Filesystem** | rootfs ro + tmpfs `/tmp` capped + `RLIMIT_FSIZE` | ❌ Sem ro, sem tmpfs-cap, sem `RLIMIT_FSIZE` | **Lacuna → risco R3/R4** |
| **Privilégio** | `NO_NEW_PRIVS` + drop caps | ❌ Não implementado | **Lacuna → F-B** |
| **Tempo (TTL)** | `--time_limit` + context + SIGKILL-pgroup | ✅ Implementado; timeout → exit −1 | **Conforme** |

**Resumo:** 4/8 conformes. As 4 lacunas são exatamente o escopo do F-B (nsjail).
A **exceção que não pode esperar** é PIDs (abaixo).

### 1.2 `RUNNER_MAX_PROCESSES` + conflito UID(1000) rootless

**Diagnóstico:** Não é defesa suficiente; é uma **auto-negação de serviço ativa e confirmada**.

**Mecânica:**
- `RLIMIT_NPROC` é contado **por uid real do kernel**, não por processo.
- Rootless single-uid → todos os runs + o próprio Go compartilham o mesmo pool NPROC.
- `LimitProcesses(256)` rebaixa esse teto **globalmente** no pai Go.
- Quando apetite de threads Go + forks dos runs encostam em 256 → `clone()` retorna `EAGAIN` → `fatal error: newosproc`.

**Prova empírica (turno anterior):** binário morreu no boot dentro do container:
```
runtime: failed to create new OS thread (have 6 already; errno=11)
fatal error: newosproc
```

Subiu apenas com `RUNNER_MAX_PROCESSES=200000`.

**Duas faces do conflito UID(1000) — não confundir:**

| Face | Onde dói | Vale no Railway? |
|---|---|---|
| **A. uid do container = uid ocupado do host** | Só Docker local (sem userns-remap) | **Não** — Railway isola; uid 1000 lá ≈6 procs |
| **B. Todos jails + Go compartilham uid 1000** | Estrutural do rootless single-uid | **Sim, em qualquer lugar** |

A face **B** é a que condena o design: mesmo no Railway, um `RLIMIT_NPROC` global (a)
é baixo demais → estrangula Go, (b) alto demais → não isola um run do outro. **O
parâmetro não tem valor satisfazendo ambas metas.**

**Contenção de fork bomb por `RLIMIT_NPROC` global é a ferramenta *errada* para rootless.**

O que *funciona* já existe: wall-timeout + `RLIMIT_CPU` + **SIGKILL pgroup** matam
fork bomb em segundos (verificado; threat model de estudante = baixo incentivo).
Contenção *dura* por-run vem do `--rlimit_nproc` **dentro do jail** (F-B) + `pids.max`
(só VPS).

**Correção imediata (F-A, destrava deploy):**
- Remover `sandbox.LimitProcesses(cfg.MaxProcesses)` de [cmd/runner/main.go:44](../cmd/runner/main.go#L44)
  (ou elevar muito acima do apetite Go).
- **Bound de concorrência no próprio runner:** semáforo stdlib de `RUNNER_MAX_CONCURRENT`,
  retorna `503` quando saturado. (Hoje: grep confirmou que runner não tem concorrência cap.)
- Deploy: `--pids-limit`/`--ulimit nproc` com folga no alvo.
- Confirmar `GOMAXPROCS` — Go 1.26 já ajusta pela quota de CPU do cgroup por padrão
  (redundando a recomendação `go.uber.org/automaxprocs` do §4.1).

### 1.3 Riscos residuais (ranqueados por impacto no threat model real)

| # | Risco | Gatilho | Severidade | Mitigação |
|---|---|---|---|---|
| **R1** | **Auto-DoS por RLIMIT_NPROC global** (§1.2) | Qualquer boot em uid povoado / rajada concorrente | **Alta (confirma)** | **F-A imediato** |
| **R3** | **`/tmp` sem cap + sem `RLIMIT_FSIZE`** → OOM host se tmpfs | `write('x'*BIG)` loop; mmap em `/dev/shm` | **Alta** | `--tmpfsmount /tmp:size=<n>m` + `--rlimit_fsize` (F-B) |
| **R2** | **Sem seccomp** — superfície syscall inteira num escape | Escape CPython (bug interpretador) | **Média** | seccomp kafel (F-B) |
| **R4** | **Rootfs gravável + sem `NO_NEW_PRIVS`** | Escrita em paths graváveis; setuid | **Média-baixa** | ro-rootfs + `NO_NEW_PRIVS` (F-B) |
| **R5** | **Runner sem backpressure própria** | Rajada de requisições | **Média (amplia R1)** | Semáforo + `503` (F-A) |
| **R6** | **`memory_exceeded` por heurística substring** ("MemoryError" em stderr) | Aluno imprime "MemoryError" (falso-positivo); OOM-kill sem texto | **Baixa** | Classificar por signal/exit sob RLIMIT_AS (2.4) |

> **Calibragem ao threat model:** R1/R3 batem em **disponibilidade** sob hostilidade
> *acidental* — prioridade máxima (F-A). R2/R4 exigem intenção + 0-day de runtime:
> baixo incentivo para estudantes, corretamente adiados ao F-B. Respeita "chato de propósito".

---

## Parte 2 — Especificação do Contrato de API

### 2.1 Contrato atual

**Endpoints:**
- `POST /run` (canônico) — `language` no body
- `POST /run/python` (deprecado, compat alias) — injeta `language=python`
- `GET /healthz` (não-autenticado)

**Autenticação:** PSK com `X-Runner-Token` header, constant-time compare.

### 2.2 Request — `runnerapi.RunRequest`

| Campo JSON | Tipo | Obrig. | Regra |
|---|---|---|---|
| `language` | string | ✅ | Identificador do catálogo (`python`). Ausente/desconhecido → **400 Bad Request**. |
| `source_code` | string | ✅ | Vazio ou > `RUNNER_MAX_SOURCE_BYTES` → **400**. |
| `stdin` | string | — | Entrada padrão (vazio = sem entrada). |
| `timeout_ms` | int | — | `≤0` → default (`RUNNER_DEFAULT_TIMEOUT_MS`); clamped ao teto (`MaxTimeoutMs`). Linguagens podem declarar um piso (G6); o teto global sempre vence. |
| `memory_mb` | int | — | `≤0` → default; clamped a `MaxMemoryMB`. Piso por linguagem (G6): Java eleva pedidos < 128 MB para 128 (overhead não-heap da JVM); nunca acima do teto. |

**Forward-compatible (F-C/F-D):**
- `compile_timeout_ms` (int, — ): `0` = interpretada (padrão); > 0 = compilada.

### 2.3 Response — `runnerapi.RunResult`

| Campo JSON | Tipo | Semântica |
|---|---|---|
| `status` | enum | Ver 2.4. |
| `stdout` / `stderr` | string | Capados em `MaxOutputBytes`; se capado, sufixo `\n[output truncated]`. |
| `exit_code` | int | Exit code do processo; −1 se morto por sinal (ex.: SIGKILL). |
| `duration_ms` | int | Wall-clock da execução (exclui fila/rede). |
| `memory_kb` | int | Maxrss (pico de residência residente) ou 0 se indisponível. |
| `runtime_name` | string | Nome da linguagem de runtime (ex.: `python`). |
| `runtime_version` | string | Versão do runtime (ex.: `3.12.3`). |
| `python_version` | string | **Deprecado** — cópia de `runtime_version` para compat com adapter legado. |

**Forward-compatible (F-C/F-D):**
- `compile_output` (string): stderr do compilador (vazio se não compilada).
- `signal` (string): nome do sinal se morto (ex.: `"SIGKILL"`); vazio se exited.

### 2.4 Máquina de estados — quando o Gateway dispara fallback Judge0

**Princípio (§0.1):** resultado de código do aluno (200 OK) ≠ falha de provider (erro
Go). Fallback dispara **apenas** em `errors.Is(err, contract.ErrProviderUnavailable)`.

| Desfecho do runner | HTTP | `Status` | **Fallback Judge0?** | Racional |
|---|---|---|---|---|
| Código OK | 200 | `success` | **Não** | Terminal, determinístico. |
| Exceção/exit≠0 do aluno | 200 | `runtime_error` | **Não** | Judge0 daria o mesmo. |
| Estourou timeout do runner | 200 | `timeout` | **Não** | Desfecho do aluno; repetição inútil. |
| OOM do aluno (SIGKILL/RLIMIT_AS) | 200 | `memory_exceeded` | **Não** | Desfecho do aluno. |
| **Deadline coordenada do Gateway estourou** (pendurado) | — | N/A | **Sim** | Provider não respondeu → falha infra. |
| **Runner inalcançável / 5xx** | 000/5xx | N/A | **Sim** | Falha de provider. |
| Runner responde **JSON inválido** | 200 | N/A | ⚠️ **Não** | `ErrDecode` ≠ `ErrProviderUnavailable`; requer fix (2.4.1). |
| Runner falha init de sandbox (mkdtemp fail) | **503** | `internal_error` | ✅ **Sim** | Runner emite `503` + `Retry-After` para `internal_error` (§2.4.1 **implementado**); 5xx é fallback-elegível. |

#### 2.4.1 Dois problemas de clareza (armadilhas)

**Armadilha 1: dois "timeout" diferentes**
- `Status=timeout` (200) = código estourou wall-clock do runner → terminal, **sem fallback**.
- `context.DeadlineExceeded` = runner pendurado → **faz fallback**.
- **Garantia:** o runner deve sempre responder *antes* da deadline coordenada do Gateway
  (timeout_ms + margem_rede). Lógica de deadline no Gateway: [gateway.go](../../dalivim-backend/backend/internal/runner/gateway.go#L120).

**Armadilha 2: falha de infra que devolve 200 não faz fallback — ✅ RESOLVIDO**
- Antes: falha de init de sandbox por-run (mkdtemp fail) → `200 {status:internal_error}`,
  que o Gateway tratava como terminal → **não** caía pro fallback.
- **Implementado (`httpapi.handler.execute`):** um resultado com
  `status == internal_error` (a falha de infra do PRÓPRIO runner — o sandbox/jail
  não subiu para aquele run) agora é servido como **`503` + `Retry-After`**, não
  `200`. O corpo ainda carrega o `RunResult` para log. Desfechos determinísticos do
  aluno (`success`/`runtime_error`/`timeout`/`memory_exceeded`/`compile_error`)
  permanecem `200` — repetir não mudaria o resultado. Assim o Gateway (que já cai
  no fallback em 5xx, §2.4) passa a poder desviar em falha de infra do runner, sem
  tocar o core. Testes: `handlers_test.go`
  (`TestRun_InternalErrorIsFailover`, `TestRun_StudentOutcomesStay200`).

#### 2.4.2 Melhorias forward-compatible

**R6 — fix de `memory_exceeded` por heurística:**
- Hoje: classifica por substring `"MemoryError"` em stderr ([python.go:107](../internal/executor/python.go#L107)).
- Falso-positivo: aluno imprime "MemoryError".
- Falso-negativo: OOM-kill sem texto em stderr.
- **Fix (F-D ou junto com `RLIMIT_AS` reporting):** classificar por `signal`/exit sob
  o limite de memória. Manter heurística de string como fallback específico de Python.
  Resulta em `memory_exceeded` legítimo via sinal, não texto — sem tocar decisão de
  fallback do Gateway.

---

## Parte 3 — Roadmap de Endurecimento (integração do Nsjail)

### 3.1 O problema de interface

Hoje o executor consome do sandbox:
- `SysProcAttr()` (rlimits + netns flags)
- `CancelCmd()` (SIGKILL do pgroup)
- `MaxRSSkb()` (colhe Maxrss)

E **constrói ele mesmo** o wrapper `ulimit …; exec python3 …` com os `Cloneflags`
do netns. Resultado: **mecanismo de netns e rlimits vazaram para o executor.**
Isto torna nsjail "difícil de encaixar" — ele não expõe `Cloneflags`, ele *é* o
processo que faz `unshare`.

**Solução:** inverter a interface de "provedor de `SysProcAttr`" para **construtor de
comando**. O executor descreve *o quê* rodar (argv + limites); o sandbox decide *como*
isolar. Netns e nsjail ficam duas implementações intercambiáveis da mesma interface.

#### 3.1.1 Interface nova (agnóstica de mecanismo)

```go
// internal/sandbox/sandbox.go

type Spec struct {
    Argv       []string  // ["/usr/local/bin/python3", "-I", "/sandbox/main.py"]
    WorkDir    string    // bind → /sandbox
    TimeoutMS  int       // wall-clock timeout
    CPUSeconds int       // RLIMIT_CPU
    MemoryMB   int       // RLIMIT_AS (memória virtual)
    NProc      int       // RLIMIT_NPROC POR-RUN (não global — mata R1)
    FSizeMB    int       // RLIMIT_FSIZE (mata R3)
    Stdin      io.Reader
    Stdout     io.Writer
    Stderr     io.Writer
}

// Sandbox constrói um *exec.Cmd já isolado. Executor roda e lê ProcessState;
// não sabe se por baixo é netns (SysProcAttr) ou nsjail (argv).
type Sandbox interface {
    // Command monta o comando isolado. O executor chama Run(), Win();
    // timeout é garantido por context + cmd.Cancel + rlimit/wall do jail.
    Command(ctx context.Context, spec Spec) *exec.Cmd

    // NetworkIsolated relata se a rede está isolada (para testes/log).
    NetworkIsolated() bool

    // Name é auditabilidade no log de boot.
    Name() string // "netns" | "nsjail"
}
```

#### 3.1.2 Duas implementações satisfazendo a interface

| Aspecto | `netnsSandbox` (F-03 atual, refatorado) | `nsjailSandbox` (F-B novo) |
|---|---|---|
| `Command` retorna | `exec.CommandContext(ctx, "/bin/sh", "-c", "ulimit -v…;-t…;-f…; exec …")` + `SysProcAttr{Setpgid, Cloneflags: NEWUSER\|NEWNET, UidMappings…}` | `exec.CommandContext(ctx, nsjailBin, nsjailArgs(spec)…, "--", argv…)` + `SysProcAttr{Setpgid}` **sem Cloneflags** |
| Namespaces criados por | Go (`os/exec` no filho pré-`exec`) | binário **nsjail** (unshare interno) |
| Rlimits | `ulimit` no dash | flags `--rlimit_*` |
| Filesystem | `/tmp` sem cap | tmpfs `/tmp:size=<n>m` |
| Syscalls | nenhuma restrição | seccomp kafel (denylist) |
| Privégio | uid 1000 + netns | uid 1000 + `NO_NEW_PRIVS` |
| Cobre riscos | R1? não; R2? não; R3? não; R4? não | R1? **sim** (pids.max); R2? **sim** (seccomp); R3? **sim** (tmpfs cap); R4? **sim** (no_new_privs) |

**Benefício:** executor não muda. Puro "injection" de qual sandbox usar.

#### 3.1.3 Diff mínimo no executor

[internal/executor/python.go](../internal/executor/python.go):

```go
// Antes (hoje):
cmd := exec.CommandContext(ctx, "/bin/sh", "-c", shellCmd)
cmd.SysProcAttr = p.sandbox.SysProcAttr()  // ← conhece SysProcAttr
cmd.Cancel = sandbox.CancelCmd(cmd)
// ...

// Depois (após refactor):
spec := sandbox.Spec{
    Argv:       []string{"/usr/bin/python3", "-I", "/sandbox/main.py"},  // virá do languageSpec
    WorkDir:    workDir,
    TimeoutMS:  req.TimeoutMs,
    CPUSeconds: (req.TimeoutMs+999)/1000 + 1,
    MemoryMB:   req.MemoryMB,
    NProc:      256,  // por-run, não global
    FSizeMB:    64,   // cap de tamanho de arquivo
    Stdin:      strings.NewReader(req.Stdin),
    Stdout:     stdout,
    Stderr:     stderr,
}
cmd := p.sandbox.Command(ctx, spec)  // ← agnóstico
// ...
```

**Resultado:** após F-A (remover global `LimitProcesses`) e este refactor, nsjail é
100% opção transparente de `Configure`. Nenhuma mudança no executor.

### 3.2 `Configure` — fábrica de boot (espelha §2.5 do plano)

```go
// internal/sandbox/sandbox.go

func Configure(policy string) (Sandbox, error) {
    policy = strings.ToLower(strings.TrimSpace(policy))
    if policy == "" {
        policy = "auto"
    }

    switch policy {
    case "off":
        slog.Warn("network isolation DISABLED; fallback netns-only")
        return &netnsSandbox{netnsIsolated: false}, nil

    case "auto", "require":
        // Probe real: run /bin/true num jail nsjail completo, seccomp incluso.
        if probeNsjail() {
            slog.Info("nsjail ENABLED", "version", nsjailVersion())
            return &nsjailSandbox{...}, nil
        }
        if policy == "require" {
            return nil, errors.New("RUNNER_SANDBOX=require: nsjail indisponível; falha fechado")
        }
        slog.Warn("WARNING: nsjail indisponível; fallback netns-only")
        return &netnsSandbox{netnsIsolated: true}, nil  // netns como fallback

    default:
        return nil, fmt.Errorf("RUNNER_SANDBOX=%q inválido (esperado auto|require|off)", policy)
    }
}
```

**Preserva a filosofia:** se o sandbox exigido não sobe, o serviço morre (`require`).
Log de boot (`nsjail ENABLED` vs `fallback netns-only`) é auditável no Railway.
(Não testável em CI sem userns — exige env real.)

### 3.3 Construção do argv do nsjail (a partir do `Spec`) — resolve R1–R4

```bash
nsjail --mode o --quiet \
  --bindmount_ro / \                           # R4: rootfs read-only
  --cwd /sandbox \
  --bindmount <WorkDir>:/sandbox:ro \          # fonte só-leitura
  --tmpfsmount /tmp:size=<N>m \                # R3: /tmp gravável, RAM-backed, COM CAP
  --rlimit_as <MemoryMB> \                     # RLIMIT_AS
  --rlimit_cpu <CPUSeconds> \                  # RLIMIT_CPU
  --rlimit_nproc <NProc> \                     # R1: PID cap POR-RUN, não global
  --rlimit_fsize <FSizeMB> \                   # R3: teto de arquivo escrito
  --time_limit <wall_s> \                      # belt (suspenders = context Go + SIGKILL-pgroup)
  --disable_no_new_privs=false \               # R4: NO_NEW_PRIVS on
  --seccomp_string '<kafel denylist>' \        # R2: syscalls perigosas bloqueadas
  -- /usr/local/bin/python3 -I /sandbox/main.py
```

**Rlimits saem do dash-`ulimit` e passam a ser flags do nsjail:**
- Inclusivamente `--rlimit_nproc <N>` agora **por-run** → fork bomb contido sem NPROC
  global (mata R1).
- `/tmp` vira tmpfs **com `size=`** → correção direta do OOM que você apontou (R3).

**Seccomp denylist (§2.4 do plano):**

```kafel
POLICY sandbox {
    KILL {
        ptrace, process_vm_readv, process_vm_writev,
        mount, umount2, pivot_root, chroot,
        kexec_load, init_module, finit_module, delete_module,
        bpf, setns, unshare, clone3,
        add_key, keyctl, request_key,
        reboot, swapon, swapoff,
        open_by_handle_at, name_to_handle_at, perf_event_open
    }
}
USE sandbox DEFAULT ALLOW
```

### 3.4 Separação de threads do Go — garantia sobre `exec.Command`

**Regra dura:** o processo Go **nunca** chama `unshare`/`setns`/`clone(CLONE_NEWUSER)`.

**Hoje (netns):** correto, mas no limite.
- `os/exec` aplica `Cloneflags` no filho já forkado, pré-`exec` (async-signal-safe).
- Single-thread no ponto crítico de `fork`.
- Seguro porém frágil.

**Com nsjail (F-B):** **mais seguro**, não menos.
- Todo `unshare`/mapeamento/seccomp acontece **dentro do binário nsjail** (processo C de
  propósito único).
- Go faz apenas um `fork+exec` trivial do `nsjail`.
- **Hazard de "namespace a partir de Go multithread" some por completo.**

**Portanto, ao migrar:**
- `cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}` — **só** isto (grupo de
  processo para kill de timeout matar a árvore `nsjail`→aluno). **Sem** Cloneflags/UidMappings.
- `cmd.Cancel = CancelCmd(cmd)` (SIGKILL no `-pgid`) + `context.WithTimeout` =
  suspenders sobre `--time_limit` do nsjail.
- **Nada** de `runtime.LockOSThread`, `clone` manual, `syscall.Unshare` no runner.
- Reaping: `os/exec` `Wait` colhe nsjail; nsjail (PID 1 do seu PID-ns) colhe o aluno
  — sem zumbis.

### 3.5 Sequenciamento (amarra com F-A/F-B do plano)

1. **F-A (imediato, destrava deploy — independe do nsjail):**
   - Remover `LimitProcesses` global de [main.go:44](../cmd/runner/main.go#L44).
   - Cap de concorrência no runner (semáforo) + `503` quando saturado.
   - Confirmar `GOMAXPROCS` do Go 1.26 no boot.
   - Smoke-test do binário real sob `--pids-limit`/`--cpus=1`.
   - **Fecha R1 e R5; cura `errno=11` confirmado.**

2. **Refactor de interface (§3.1):**
   - Substituir `SysProcAttr()` → `Command(Spec)`.
   - `netnsSandbox` satisfaz nova interface (comportamento idêntico a hoje).
   - Puramente mecânico, coberto pelos testes atuais de netns/egress.

3. **F-B (nsjail):**
   - `nsjailSandbox` + `Configure`-fábrica.
   - Argv/seccomp/tmpfs-capped/no_new_privs.
   - Dockerfile from-source (nsjail + Node + gcc/g++; §4.2 do plano).
   - **Fecha R2, R3, R4; dá contenção de PID por-run real.**
   - Executor não muda.

---

## Próximos passos

- [ ] **F-A (imediato):** remover RLIMIT_NPROC global + adicionar cap de concorrência
  + smoke-test de boot. Abre o deploy.
- [ ] **Refactor de interface:** Sandbox::Command, netnsSandbox refatorado, testes verdes.
- [ ] **F-B (nsjail):** nsjailSandbox, Dockerfile, corpus de escape.
- [ ] Salvar este documento como parte da base de conhecimento do projeto.

---

**Apêndice:** checklist de deploy (do plano, Apêndice B)

- [ ] `RUNNER_SERVICE_TOKEN` forte em ambos os serviços; `RUNNER_ENV` ≠ development no runner.
- [ ] `GOMAXPROCS` = quota de CPU (Go 1.26 já faz isso automaticamente via cgroup).
- [ ] `--pids-limit`/`--ulimit nproc` com folga no alvo; sem `RLIMIT_NPROC` global rebaixado.
- [ ] `RUNNER_SANDBOX=require` num deploy de verificação → confirmar `nsjail ENABLED`;
  depois `auto` em regime.
- [ ] Sem domínio público no runner; egress bloqueado.
- [ ] `runner-smoke` e `runner-security` verdes na pipeline.
- [ ] Corpus de escape (risk matrix §1.3) contido no ambiente real.
