# Runner-Service — Plano de Arquitetura, Hardening e Evolução Poliglota

> **Status:** proposta técnica (planejamento). Não altera código de runtime — é o
> blueprint que as PRs de implementação seguem.
> **Escopo:** o executor de código não confiável (`runner-service` em Go) e a
> camada de despacho no backend (`backend/internal/runner/*`).
> **Data:** 2026-07-08.
> **Relacionados:** [`RUNNER_SECURITY.md`](RUNNER_SECURITY.md) (F-03, já entregue),
> `SECURITY_FINDINGS_AND_FIXES.md` (F-05/F-06/F-11).

---

## 0. Sumário executivo e decisões (ADR)

O sistema executa **código hostil submetido por usuários** — é uma fronteira de
RCE por design. A arquitetura de despacho já é sólida; as lacunas são de
**hardening**, **poliglotismo** e **integridade de boot no alvo**. Este plano
fecha as quatro frentes de forma faseada e sem refatorar o core.

| # | Decisão | Justificativa |
|---|---------|---------------|
| **D-1** | O `runner-service` (provider `local`) é o **único provider em produção** nos próximos meses; Judge0 permanece **plugável** atrás do mesmo contrato. | A abstração `RunnerProvider` já isola o core; trocar/adicionar provider é configuração, não refatoração. |
| **D-2** | Sandboxing via **nsjail rootless** (namespaces de usuário), **não** privileged/cgroups-obrigatório. | nsjail rootless usa o **mesmo mecanismo do netns do F-03** que já funciona no Railway; o `isolate` do Judge0 exige privileged/cgroup e **não roda no Railway**. |
| **D-3** | Contenção de fork bomb migra de um **`RLIMIT_NPROC` global** (no processo Go pai) para **limites por-sandbox** (cgroup `pids.max` onde houver; senão `RLIMIT_NPROC` no jail + limite de concorrência). | Elimina a auto-inanição do scheduler do Go que causa `errno=11` e melhora o isolamento (por-run, não global). |
| **D-4** | Poliglotismo por **`languageSpec` registry**: interpretadas (Python/Node) rodam direto no jail; compiladas (C/C++) têm **fase de compilação isolada** separada da execução. | O contrato já modela `CompileOutput`/`compile_error` e `CompileTimeoutMS`. O compilador é entrada não confiável → também vai preso. |
| **D-5** | Roteamento por-linguagem/carga/saúde entra como um **`Router` que implementa `RunnerProvider`** (decorator), encaixando no `Gateway` **sem mudar sua assinatura**. | Composição sobre modificação; o domínio continua chamando `Gateway.Run`. |
| **D-6** | Integridade de boot é garantida por **smoke tests do binário Go real** (não mocks contratuais) na pipeline, no ambiente-alvo de recursos. | O `errno=11` provou que "verde com mocks" ≠ "sobe no alvo". |

### 0.1 Estado atual (o que já existe — não reinventar)

Confirmado por leitura do código (`backend/internal/runner/*`, `runner-service/src/main.go`):

- **Strategy pronto:** `contract.RunnerProvider` (`Name/Run/ListRuntimes/Health`).
  `Run` **nunca** transforma erro de código do aluno em erro Go — timeout/erro de
  runtime/compile viram `RunResult.Status`; erro Go é só falha do **provider**.
- **Gateway** (`runner/gateway.go`) já centraliza política Uruquim: gate de
  linguagem (`enabled`), clamp de limites (`RuntimePolicy.Resolve`), deadline
  coordenado (`timeout + margem`), **backpressure** por semáforo (`RUNNER_MAX_CONCURRENT`),
  truncamento de saída, normalização de status, fallback e métricas.
- **Factory** `providers.New(Config)` seleciona `local`|`judge0` por env; valida
  requisitos no boot (judge0 exige `JUDGE0_BASE_URL`).
- **Contrato já é poliglota no vocabulário:** `Language` ∈ {python, javascript, c,
  cpp, java}; `RunRequest.Language`; `RunResult.CompileOutput`/`StatusCompileError`;
  `RuntimePolicy.PerLanguage[...].CompileTimeoutMS` (C/C++ = 10s, Java = 15s).
- **Lacuna concentrada no `local`:** o adapter (`runner/local/adapter.go`)
  **rejeita tudo que não é Python** e chama `/run/python`; o `runner-service`
  executa `python3 -I main.py` num único endpoint. O isolamento (netns, rlimits,
  timeout, cap de output) **já envolve o exec** e portanto é agnóstico de linguagem.

**Consequência de projeto:** quase todo o trabalho poliglota e de hardening vive
**dentro do `runner-service` e do adapter `local`**. O core (domínio → Gateway →
contrato) praticamente não muda. É isso que torna o plano barato.

---

## 1. Padrão de alternância (Gateway / Strategy)

### 1.1 O Strategy que já temos

`contract.RunnerProvider` é o ponto de extensão. O domínio (`submissions`) depende
**apenas** do `Gateway`, que depende **apenas** do contrato. Adicionar Judge0 no
futuro não toca uma linha do domínio. **Não há o que refatorar aqui — só a estender.**

### 1.2 O Gateway como dispatcher de política

O `Gateway` é o "contexto" do Strategy e o dono de tudo que é política (não
comportamento de provider): gate, limites, deadline, concorrência, fallback,
normalização. Mantê-lo assim é a regra: **nenhuma lógica específica de provider
sobe pro Gateway.**

### 1.3 Seleção por ambiente (já implementado)

```
RUNNER_PROVIDER=local            # default; o runner-service
RUNNER_FALLBACK_PROVIDER=        # vazio; opcional local|judge0
```
`cmd/api` monta `providers.Config` do app config e chama `providers.New`. Trocar
pra Judge0 no futuro = mudar env + prover `JUDGE0_BASE_URL`. Zero deploy de código.

### 1.4 O que falta: `Router` (roteamento por linguagem / carga / saúde)

Hoje o `Gateway` tem **um** provider primário + **um** fallback. A "alternância
dinâmica e transparente" que você quer (ex.: Python no `local`, C++ no Judge0;
ou desviar sob carga) é um **problema de seleção**, não de despacho. A solução
limpa **não muda o Gateway**: introduza um provider composto que **implementa o
próprio `RunnerProvider`** e escolhe o backend por request. Como o Gateway aceita
um `contract.RunnerProvider`, o `Router` encaixa sem alterar assinatura alguma
(padrão decorator/composite).

```go
// backend/internal/runner/routing/router.go  (novo pacote — evita ciclo)
package routing

import (
	"context"
	"github.com/uruquim/uruquim/backend/internal/runner/contract"
)

// Rule decide o provider de um request. Ordem: primeira que casar vence.
type Rule func(req contract.RunRequest) (provider string, matched bool)

// Router implementa contract.RunnerProvider compondo N providers nomeados e
// escolhendo um por request via regras (linguagem, flags, carga/saúde). Para o
// Gateway, é indistinguível de um provider único.
type Router struct {
	providers map[string]contract.RunnerProvider // "local", "judge0"
	rules     []Rule
	fallback  string // provider usado quando nenhuma regra casa
	health    HealthGate // opcional: pula provider degradado
}

func (r *Router) Name() string { return "router" }

func (r *Router) Run(ctx context.Context, req contract.RunRequest) (contract.RunResult, error) {
	name := r.pick(req)
	p, ok := r.providers[name]
	if !ok {
		return contract.RunResult{}, contract.ErrProviderUnavailable
	}
	return p.Run(ctx, req)
}

func (r *Router) pick(req contract.RunRequest) string {
	for _, rule := range r.rules {
		if name, ok := rule(req); ok && r.health.OK(name) {
			return name
		}
	}
	return r.fallback
}
```

Regras dirigidas por **feature flags / env**, sem recompilar a lógica de negócio:

```
# Roteamento declarativo (parseado no cmd/api → []Rule)
RUNNER_ROUTES="cpp=judge0,c=judge0,*=local"
```

**Contrato de estabilidade (a garantia que você pediu):** o `Gateway.Run` e a
interface `RunnerProvider` são **imutáveis**. Adicionar Judge0, rotear por
linguagem, ou desviar sob carga são todos: (a) novos providers registrados no
`Router`, ou (b) novas `Rule`s. O domínio e o Gateway **nunca** enxergam isso.

> **Faseamento:** o `Router` só é necessário quando existir um segundo provider
> ativo. Nos próximos meses (só `local`), ele é **desnecessário** — `providers.New`
> devolve o `local` direto. Implemente o `Router` **na PR que ativar o Judge0**,
> não antes (YAGNI). Este plano fixa o desenho pra que essa PR seja aditiva.

---

## 2. Segurança e Nsjail (implementação rigorosa)

### 2.1 Threat model

Entrada: **código arbitrário e hostil**. Objetivo: um escape de linguagem
(ex.: bug no CPython, binário C malicioso) encontra uma **cela nua** — sem rede,
sem filesystem gravável útil, sem segredos, com syscalls perigosas mortas, CPU/mem/
tempo/PIDs limitados — em vez do container. Defesa em profundidade: nenhuma camada
é o único ponto de falha.

### 2.2 Por que nsjail rootless roda no Railway (e o isolate do Judge0 não)

- **nsjail rootless** cria os namespaces via **user namespace não privilegiado**
  (`CLONE_NEWUSER` mapeando o uid do runner → 0 dentro do namespace). É
  **exatamente** o mecanismo do F-03 (`CLONE_NEWUSER|CLONE_NEWNET`) que **já provou
  funcionar no Railway** (o probe de netns passa). A partir daí nsjail adiciona
  mount/PID/IPC/UTS ns, rootfs read-only e seccomp — **tudo rootless**.
- **cgroups são opcionais** no nsjail. As features de cgroup (`--cgroup_pids_max`
  etc.) exigem um cgroup **delegado** — o que o Railway **não** dá. Mas os limites
  equivalentes via **rlimit** (`--rlimit_nproc/_as/_cpu/_fsize`) funcionam rootless.
- O **isolate do Judge0** exige container `--privileged` + controle direto de
  cgroups v1 no host → **incompatível com Railway** (confirmado: erros
  `Failed to create control group /sys/fs/cgroup/...`).

**Regra de deploy:** onde houver cgroup delegado (VPS/host próprio) → usar cgroups
(isolamento superior de PIDs/mem). No Railway → usar rlimits. O código detecta e
escolhe (probe no boot, igual ao F-03).

### 2.3 As camadas exatas

| Camada | Railway (rootless) | Host privilegiado (VPS) | Contém |
|--------|--------------------|--------------------------|--------|
| **Rede** | netns vazio (nsjail cria por default) | idem | egress, metadata cloud (169.254.169.254) |
| **Syscalls** | seccomp kafel (denylist) | seccomp kafel | `ptrace`, `mount`, `bpf`, `unshare`, `keyctl`, module load, etc. |
| **PIDs (fork bomb)** | `--rlimit_nproc` no jail + limite de concorrência | **cgroup `pids.max`** por run | fork bombs |
| **Memória** | `--rlimit_as` | cgroup `memory.max` | OOM do host |
| **CPU** | `--rlimit_cpu` + wall `--time_limit` | cgroup `cpu.max` + wall | loops infinitos |
| **Filesystem** | bind-mount `/` **read-only** + tmpfs `/tmp` + workdir bind | idem | leitura de segredos, escrita no host |
| **Privilégio** | `NO_NEW_PRIVS` + drop de todas as caps | idem | escalada via setuid |
| **Tempo (TTL)** | `--time_limit` (nsjail) + `context.WithTimeout` (Go) + SIGKILL no process-group | idem | travamento |

### 2.4 Invocação nsjail + política seccomp (concreto)

Política kafel — **denylist** (default `ALLOW`, mata as perigosas). Allowlist é
mais forte porém quebra fácil o CPython/Node (superfície enorme); comece denylist
e aperte por-linguagem (binário C estático tolera allowlist restrita — ver §3.4).

```
# runner-service: policy embutida (--seccomp_string) ou arquivo .kafel
POLICY sandbox {
	KILL {
		ptrace, process_vm_readv, process_vm_writev,
		mount, umount2, pivot_root, chroot,
		kexec_load, init_module, finit_module, delete_module,
		bpf, setns, unshare, clone3,        # clone3 se a política de clone exigir
		add_key, keyctl, request_key,
		reboot, swapon, swapoff,
		open_by_handle_at, name_to_handle_at, perf_event_open
	}
}
USE sandbox DEFAULT ALLOW
```

Args (montados em Go; `--` separa o comando do aluno):

```
nsjail \
  --mode o --quiet \
  --bindmount_ro / \                 # rootfs inteiro read-only (agnóstico de arch)
  --tmpfsmount /tmp \                # /tmp gravável, isolado por-run (resolve F-11)
  --bindmount <workdir>:/sandbox \   # fonte + artefatos do run
  --cwd /sandbox \
  --rlimit_as <mem_mb> \             # RLIMIT_AS em MB
  --rlimit_cpu <cpu_s> \             # RLIMIT_CPU em s
  --rlimit_nproc <nproc> \           # fork bomb (ver ressalva §2.6)
  --rlimit_fsize <fsize_mb> \        # tamanho máx de arquivo escrito
  --time_limit <wall_s> \            # wall-clock (belt do context Go)
  --seccomp_string '<policy acima>' \
  -- /usr/local/bin/python3 -I /sandbox/main.py
```

**Onde houver cgroup v2 delegado** (VPS), troque os `--rlimit_nproc`/`_as`/`_cpu`
por `--use_cgroupv2 --cgroupv2_mount <path> --cgroup_pids_max=<n> --cgroup_mem_max=<bytes> --cgroup_cpu_ms_per_sec=<n>`.

### 2.5 Política `RUNNER_SANDBOX=auto|require|off` + probe (espelha o F-03)

O nsjail é aplicado atrás de um dial de operador com **probe no boot** — se o
ambiente não permitir (kernel sem userns, binário ausente, seccomp rejeitado), o
serviço **não quebra**: cai pro caminho netns-only do F-03 (`auto`) ou falha
fechado (`require`).

```go
// runner-service/src/main.go (esboço; integra com o configureNetworkIsolation existente)
func configureSandbox() {
	switch policy := getenvDefault("RUNNER_SANDBOX", "auto"); policy {
	case "off":
		log.Printf("nsjail DISABLED; netns-only (F-03)")
	case "auto", "require":
		bin, err := exec.LookPath("nsjail")
		if err == nil {
			if ok, detail := probeNsjail(bin); ok {  // roda /bin/true no jail real, seccomp incluso
				nsjailBin, _ = bin, nsjailActive.Store(true)
				log.Printf("nsjail ENABLED (ro-rootfs, ns mount/pid/ipc, netns, tmpfs /tmp, seccomp)")
				return
			} else if policy == "require" {
				log.Fatalf("RUNNER_SANDBOX=require: probe do nsjail falhou: %s", detail)
			} else {
				log.Printf("WARNING: nsjail indisponível (%s); fallback netns-only", detail)
			}
		} else if policy == "require" {
			log.Fatal("RUNNER_SANDBOX=require mas nsjail não instalado")
		} else {
			log.Printf("WARNING: nsjail ausente; fallback netns-only")
		}
	default:
		log.Fatalf("RUNNER_SANDBOX inválido: %q", policy)
	}
}
```

> **Validação no alvo é obrigatória:** nsjail **não é testável em CI** sem o binário
> e sem userns. O log de boot (`nsjail ENABLED` vs `fallback`) é a fonte de verdade
> no Railway; use `RUNNER_SANDBOX=require` num deploy de verificação pra provar que
> engatou (falha fechado e imprime o erro do nsjail). Testes unitários cobrem a
> **construção dos args** e a **máquina de estados do config** (puras), não o jail.

### 2.6 Ressalva crítica de PIDs no rootless (liga com o `errno=11`)

`RLIMIT_NPROC` é contado **por uid real do kernel**. No rootless single-uid do
Railway, **todos os jails concorrentes mapeiam pro mesmo uid do runner** → eles
**compartilham o mesmo pool de NPROC** entre si e com o processo Go pai. Ou seja:
`--rlimit_nproc` no jail **não isola** um run do outro de forma perfeita, e um
`RLIMIT_NPROC` global baixo no pai **estrangula o scheduler do Go** (a causa do
`errno=11` — §4.1).

Consequências de projeto (D-3):

- **NÃO** rebaixar `RLIMIT_NPROC` no processo Go pai (remover o `limitProcesses()`
  global do F-03, ou elevá-lo bem acima do apetite do Go).
- Contenção de fork bomb **por-run** só é perfeita com **cgroup `pids.max`** (VPS).
  No Railway, contém-se por: (a) `--rlimit_nproc` no jail como teto grosseiro +
  (b) **limite de concorrência** (`RUNNER_MAX_CONCURRENT`, já existe) bounded, de
  modo que `Go_threads + concorrência × nproc_por_run < teto_global_seguro`, +
  (c) `RLIMIT_CPU` + wall-timeout + **SIGKILL no process-group** matam a bomba em
  segundos.
- Distinct-uid-por-run (isolamento perfeito rootless) exigiria `/etc/subuid` +
  `newuidmap` (precisa de setuid ou root) — **inviável no Railway rootless**;
  fica documentado como upgrade no caminho VPS.

---

## 3. Engenharia de runtimes (pipeline poliglota)

### 3.1 Evolução do protocolo de wire

Hoje: `POST /run/python` com `{source_code, stdin, timeout_ms, memory_mb}`.
Alvo: **endpoint genérico** com a linguagem no corpo (mantendo `/run/python` como
alias deprecado por uma release, pra deploy sem downtime).

```
POST /run
{
  "language": "cpp",              // contract.Language
  "source_code": "...",
  "stdin": "...",
  "timeout_ms": 5000,
  "memory_mb": 256,
  "compile_timeout_ms": 10000     // 0 = interpretada
}
=>
{
  "status": "success|compile_error|runtime_error|timeout|memory_exceeded|internal_error",
  "stdout": "...", "stderr": "...",
  "compile_output": "...",        // vazio p/ interpretadas
  "exit_code": 0, "signal": "",
  "duration_ms": 12, "memory_kb": 2048,
  "runtime_version": "gcc 12.2"
}
```

No backend, o adapter `local` deixa de rejeitar não-Python e passa a mapear
`req.Language` → `/run`. **O contrato e o Gateway não mudam** — só o adapter e o
DTO de wire (`runner/dto.go`) ganham `Language`/`CompileOutput`/`Signal`.

### 3.2 Registry de `languageSpec` (runner-service)

O coração do poliglotismo. Uma spec por linguagem descreve arquivo-fonte, comando
de compilação (opcional) e comando de execução. **Fechado por design** (adicionar
linguagem = code review deliberado, igual ao catálogo do contrato).

```go
// runner-service/src/languages.go
type languageSpec struct {
	name       string        // "python", "javascript", "c", "cpp"
	sourceFile string        // "main.py", "main.js", "main.c", "main.cpp"
	// compile: nil p/ interpretadas. Roda num jail SEPARADO (§3.4). Placeholders:
	// {src} = caminho do fonte no jail, {out} = caminho do artefato no jail.
	compile []string
	// run: comando final no jail de execução. {out} = artefato compilado.
	run []string
}

var specs = map[string]languageSpec{
	"python": {
		name: "python", sourceFile: "main.py",
		run: []string{"/usr/local/bin/python3", "-I", "/sandbox/main.py"},
	},
	"javascript": {
		name: "javascript", sourceFile: "main.js",
		// --disable-proto=throw fecha prototype pollution; sem rede via netns.
		run: []string{"/usr/bin/node", "--disable-proto=throw", "/sandbox/main.js"},
	},
	"c": {
		name: "c", sourceFile: "main.c",
		compile: []string{"/usr/bin/gcc", "-O2", "-static", "-o", "{out}", "{src}"},
		run:     []string{"{out}"},
	},
	"cpp": {
		name: "cpp", sourceFile: "main.cpp",
		compile: []string{"/usr/bin/g++", "-O2", "-static", "-std=c++20", "-o", "{out}", "{src}"},
		run:     []string{"{out}"},
	},
}
```

### 3.3 Interpretadas (Python / JavaScript) — injeção segura

Uma fase: escreve o fonte em `workdir/main.<ext>` (perm `0600`), monta `workdir`
read-only (ou o binário lê e o programa escreve só em `/tmp` tmpfs), roda o `run`
da spec no jail. **O fonte nunca entra na linha de comando** (vai pro arquivo) —
zero injeção de shell. Node ganha `--disable-proto=throw` e herda o mesmo jail
(sem rede, seccomp, mem/cpu/tempo). Sem `npm install`, sem acesso a `node_modules`
externo: execução de arquivo único.

### 3.4 Compiladas (C/C++) — compilação isolada + execução mínima

O ciclo tem **duas fases em dois jails distintos**, porque **o compilador é entrada
não confiável** (bombas de template/macro, `#include` recursivo, preprocessor
abuse) e um vetor de ataque por si só.

**Fase 1 — Compilar (jail do compilador):**
- Jail próprio: **sem rede**, rootfs read-only, tmpfs gravável só pro build+artefato,
  `RLIMIT_CPU`/`RLIMIT_AS`/`--time_limit` = `compile_timeout_ms` e mem de compilação,
  `RLIMIT_FSIZE` cap no artefato.
- `gcc -O2 -static -o {out} {src}`: `-static` evita o dynamic loader no runtime
  (menos superfície, jail de execução mais pobre pode rodar o binário sem `/lib`).
- O read-only do jail já impede o compilador de ler segredos do host; caps de
  CPU/mem/tempo domam bombas de compilação. `compile_output` (stderr do gcc) volta
  como `StatusCompileError` quando o exit ≠ 0 — **sem** rodar nada.
- Validar o artefato: existe? tamanho ≤ cap? Senão `internal_error`.

**Fase 2 — Executar (jail de execução, mais estrito):**
- Um binário **estático** faz pouquíssimas syscalls → aqui cabe uma **seccomp
  allowlist** apertada (read/write/exit/brk/mmap/rt_sigreturn/…), muito mais forte
  que a denylist das interpretadas.
- Jail novo, read-only, sem rede, tmpfs `/tmp`, `RLIMIT_*` de execução (não de
  compilação). O artefato entra read-only e é o único executável.
- Nenhum compilador, header ou toolchain presente no jail de execução (bind só do
  necessário) → o aluno não pode recompilar/parsear em runtime.

```go
// runner-service: orquestração compile→run (esboço)
func runCompiled(spec languageSpec, req runRequest) runResult {
	build := mkTmp();      defer os.RemoveAll(build)
	artifact := filepath.Join(build, "bin")
	// Fase 1: compilar preso
	cres := execInJail(compileJailArgs(build), subst(spec.compile, req.sourcePath, artifact), req.compileTimeout)
	if cres.exit != 0 {
		return runResult{Status: "compile_error", CompileOutput: cres.stderr}
	}
	if !fileWithinSize(artifact, maxArtifactBytes) {
		return runResult{Status: "internal_error"}
	}
	// Fase 2: executar preso (jail diferente, seccomp mais estrito)
	rres := execInJail(runJailArgs(build, artifact), subst(spec.run, "", artifact), req.timeout)
	return classify(rres)
}
```

**Java** (fase 2 do plano): `javac` (compila p/ bytecode) + `java` na JVM. A JVM
cria muitas threads → casa com o cuidado de `RLIMIT_NPROC`/`pids.max` (§2.6) e
custa +300MB de imagem (JDK) — só adicionar quando houver demanda real.

### 3.5 Mudanças necessárias (checklist de implementação)

- `runner/dto.go`: `RunRequest` ganha `Language`, `CompileTimeoutMs`; `RunResult`
  ganha `CompileOutput`, `Signal`.
- `runner/client.go`: `RunPython` → `Run` genérico em `POST /run` (mantém alias).
- `runner/local/adapter.go`: remove o reject de não-Python; mapeia `req.Language`,
  `CompileOutput`, `Signal`; `ListRuntimes` passa a refletir o registry.
- `runner-service`: `/run` genérico + `languageSpec` registry + `execInJail` +
  fase compile/run.
- `RUNNER_LANGUAGES_ENABLED=python,javascript,c,cpp` habilita no Gateway
  (o gate já existe; só ligar).
- Dockerfile do runner: instalar `nodejs`, `gcc/g++` (build-essential), nsjail.

---

## 4. Roadmap de deploy, tuning do host e mitigação de riscos

### 4.1 `errno=11` — diagnóstico rigoroso e correção

`failed to create new OS thread (errno=11)` = o runtime do Go chamou `clone()` e
recebeu **`EAGAIN`**. Não é bug do Go — é **limite de recurso** batido. Causas
possíveis (checar nesta ordem):

| Causa | Como checar | Sintoma |
|-------|-------------|---------|
| `RLIMIT_NPROC` do uid (ulimit -u) | `ulimit -u` | baixo, ou uid compartilhado no host já perto do teto |
| cgroup `pids.max` | `cat /sys/fs/cgroup/pids.max` | container com `--pids-limit` apertado |
| kernel `threads-max` / `pid_max` | `cat /proc/sys/kernel/{threads-max,pid_max}` | host exausto (raro) |
| **GOMAXPROCS = nº de cores do host** | `nproc` vs quota real | Go num container 1-vCPU num host 64-core cria Ms demais |
| `limitProcesses()` global (F-03) | código | rebaixa `RLIMIT_NPROC` no pai Go |

Referência empírica (ambiente restrito de exemplo): `ulimit -u=95041`,
`nproc=8`, `threads-max=190082`, `pids.max=n/a (sem cgroup delegado)`. Num host
grande com container pequeno, `nproc` reportaria os cores do **host**, não a quota
— é aí que mora a armadilha.

**Correções (todas entram no plano):**

1. **Fixar `GOMAXPROCS` à quota real** — a causa mais comum em container. Adotar
   [`go.uber.org/automaxprocs`](https://github.com/uber-go/automaxprocs) (lê o
   cgroup CPU e ajusta) **ou** setar `GOMAXPROCS` no deploy. Sem isso o Go acha
   que tem os cores do host e infla o apetite de threads.
2. **Não estrangular o pai** (D-3) — remover o `limitProcesses()` global ou
   elevá-lo bem acima de `Go_threads + concorrência × nproc_por_run`. Contenção de
   fork bomb vai pro jail (§2.6).
3. **Dimensionar o container no alvo:** `--ulimit nproc=<n>` e `--pids-limit=<n>`
   com folga; garantir headroom de memória (cada thread reserva ~8MB de stack
   virtual).
4. **Bound de concorrência:** `RUNNER_MAX_CONCURRENT` calibrado, pra que o pior
   caso (N runs × threads/run) caiba no teto.

### 4.2 Dockerfile do runner (multi-stage: nsjail + runtimes)

nsjail **não é apt confiável** no Debian → compilar do fonte num stage bookworm e
copiar o binário + libs de runtime; imagem final pinada em `bookworm` (ABI de
`libprotobuf32`/`libnl-route-3-200` casa).

```dockerfile
FROM golang:1.26-alpine AS build
WORKDIR /app
COPY go.mod ./ ; COPY src/ ./src/
RUN CGO_ENABLED=0 go build -o /runner ./src

FROM debian:bookworm-slim AS nsjail-build
ARG NSJAIL_VERSION=3.4
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git autoconf bison flex gcc g++ libtool make pkg-config \
      libprotobuf-dev libnl-route-3-dev protobuf-compiler \
 && rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 --branch "${NSJAIL_VERSION}" https://github.com/google/nsjail /nsjail \
 && make -C /nsjail

FROM python:3.12-slim-bookworm
# Runtimes poliglotas + libs de runtime do nsjail. (Só o que você ensina.)
RUN apt-get update && apt-get install -y --no-install-recommends \
      nodejs gcc g++ libc6-dev \
      libprotobuf32 libnl-route-3-200 \
 && rm -rf /var/lib/apt/lists/*
RUN useradd --create-home --shell /usr/sbin/nologin runner
COPY --from=build       /runner        /usr/local/bin/runner
COPY --from=nsjail-build /nsjail/nsjail /usr/local/bin/nsjail
USER runner
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/runner"]
```

> **Custo (honesto):** +stage de build do nsjail (descartado) + Node/gcc/g++
> incham a imagem (~+300–500MB) + ~5–20ms/exec de setup do jail. Adicione **só as
> linguagens que você usa**. JDK fica de fora até haver demanda.

### 4.3 Smoke tests de boot no ambiente-alvo (binário real, não mocks)

O `errno=11` provou a lição: **CI verde com mocks contratuais ≠ o binário Go sobe
no alvo.** A pipeline precisa exercer o **binário/imagem reais** sob os limites do
alvo.

```yaml
# .github/workflows/ci.yml — job novo: runner-smoke
runner-smoke:
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4
    - name: Build runner image
      run: docker build -t runner:smoke ./runner-service
    - name: Boot sob limites do alvo (reproduz a restrição de recursos)
      run: |
        docker run -d --name runner \
          --pids-limit=512 --ulimit nproc=512 --cpus=1 --memory=512m \
          -e RUNNER_ENV=development -e RUNNER_SANDBOX=auto \
          -e GOMAXPROCS=1 -p 8090:8090 runner:smoke
    - name: Boot íntegro (healthz responde em <10s)
      run: for i in $(seq 1 20); do curl -fs localhost:8090/healthz && break; sleep 0.5; done
    - name: Execução REAL por linguagem (não mock)
      run: |
        ./scripts/smoke-run.sh python     'print(2+2)'            '4'
        ./scripts/smoke-run.sh javascript 'console.log(2+2)'     '4'
        ./scripts/smoke-run.sh c          'int main(){puts("4");}' '4'
    - name: Logs de boot devem confirmar sandbox
      run: docker logs runner 2>&1 | grep -E "nsjail (ENABLED|indisponível)|network isolation"
```

`scripts/smoke-run.sh` faz `POST /run`, compara `stdout` e **falha o job** se o
status não for `success` — garantindo que compile→run e o jail funcionam de fato.

### 4.4 Resiliência contínua — corpus de "malware" de teste

Um conjunto versionado de submissões adversariais + o contido esperado, rodado no
CI (`runner-security`) e periodicamente. Cada uma **deve** ser contida:

| Ataque | Submissão | Contido esperado |
|--------|-----------|-------------------|
| Egress | `socket.create_connection(("1.1.1.1",80))` | `Network is unreachable` (netns) |
| Metadata cloud | conectar `169.254.169.254:80` | bloqueado |
| Fork bomb | `while True: os.fork()` | morto por `nproc`/`pids.max`, host vivo |
| CPU spin | `while True: pass` | `timeout` no wall/CPU limit |
| Memory bomb | `b"x"*10**10` | `memory_exceeded` (RLIMIT_AS) |
| Leitura de segredo | ler `/etc/shadow`, envs do host | negado (ro/jail, env mínimo) |
| Escrita no host | escrever em `/usr`, `/app` | read-only ⇒ falha |
| Syscall perigosa | `ptrace`, `mount`, `unshare` | morto por seccomp |
| Output flood | `print("x"*10**9)` | truncado no cap, sem OOM |
| Compiler bomb (C++) | template/macro explosivo | `compile_error` no timeout de compilação |

Além disso: **stress/concorrência** — N submissões simultâneas no limite de
`RUNNER_MAX_CONCURRENT` para provar backpressure (`503 runner_busy`) e ausência de
vazamento de PIDs/tmp entre runs.

### 4.5 Gates de CI/CD (aditivos ao F-16 já entregue)

O F-16 já trouxe `govulncheck`, `gosec`, `gitleaks` e inventário de rotas.
Acrescentar: **(a)** `runner-smoke` (§4.3), **(b)** `runner-security` (corpus §4.4),
**(c)** trivy/grype na imagem do runner (CVEs de Node/gcc/nsjail), **(d)** verificação
de que o log de boot confirma `nsjail ENABLED` em ambiente que o suporta.

### 4.6 Plano de contingência de performance (antes do Judge0)

Se o `runner-service` virar gargalo antes da integração do Judge0:

1. **Escala horizontal** — o runner é **stateless**; suba N réplicas atrás do
   `RUNNER_SERVICE_URL` (load balancer do Railway). O `Gateway` já limita
   concorrência por instância do backend; ajuste `RUNNER_MAX_CONCURRENT`.
2. **Backpressure já existe** — `503 runner_busy` protege o serviço; o front deve
   tratar e re-tentar com backoff.
3. **Válvula de escape pro Judge0** — quando o `Router` (§1.4) existir, desviar as
   linguagens/carga problemáticas pro Judge0 (num VPS) é **só uma `Rule` + env**,
   sem tocar o core. Essa é a razão de manter o contrato estável.
4. **Cost guardrails** — `RUNNER_MAX_TIMEOUT_MS`, `RUNNER_MAX_MEMORY_MB` e o cap de
   concorrência bounded impedem que uma rajada de execuções caras derrube custo/host.

---

## 5. Roadmap faseado

| Fase | Entrega | Depende de | Critério de aceite |
|------|---------|------------|--------------------|
| **F-A** | **Fix `errno=11`**: automaxprocs + remover `RLIMIT_NPROC` global + `smoke-run` de boot no CI sob limites | — | boot verde sob `--pids-limit`/`--cpus=1`/`GOMAXPROCS=1` |
| **F-B** | **nsjail** (`RUNNER_SANDBOX=auto`, probe, args, seccomp, Dockerfile from-source) — F-05/F-11 | F-A | corpus de escape contido; log `nsjail ENABLED` no alvo |
| **F-C** | **Poliglota interpretado** (Node) — `/run` genérico, `languageSpec`, adapter sem reject | F-B | smoke JS verde; JS herda o jail |
| **F-D** | **Poliglota compilado** (C/C++) — compile-jail + run-jail, seccomp estrita p/ binário | F-C | compile_error correto; corpus C contido |
| **F-E** | **Endurecimento contínuo** — `runner-security` no CI, trivy, stress/concorrência | F-B | pipeline bloqueia regressão de contenção |
| **F-F** *(futuro, sob demanda)* | **Router + Judge0** — roteamento por env, Judge0 num VPS privilegiado | contrato estável | trocar provider por linguagem sem tocar o core |

**Ordem recomendada:** F-A **imediato** (destrava o deploy), depois F-B → F-C → F-D
em PRs separadas, F-E em paralelo a partir de F-B. F-F só quando a matriz de
linguagens ou a carga justificarem o custo do VPS.

---

## Apêndice A — Matriz de limites por linguagem (da `RuntimePolicy`)

| Linguagem | Timeout run | Memória | Timeout compile | Fase compile |
|-----------|-------------|---------|-----------------|--------------|
| python | 3s | 128MB | — | não |
| javascript | 3s | 128MB | — | não |
| c | 5s | 256MB | 10s | sim |
| cpp | 5s | 256MB | 10s | sim |
| java *(futuro)* | 8s | 384MB | 15s | sim |

Caps globais: `MaxTimeoutMS=10s`, `MaxMemoryMB=512`, `MaxOutputBytes=64KB`,
`MaxSourceBytes=200KB`. O `Gateway` faz clamp antes de despachar; o adapter só
converte unidades.

## Apêndice B — Checklist de deploy do runner (alvo)

- [ ] `RUNNER_SERVICE_TOKEN` forte nos dois serviços; `RUNNER_ENV` ≠ development no runner.
- [ ] `GOMAXPROCS` = quota de CPU (ou automaxprocs no binário).
- [ ] `--pids-limit`/`--ulimit nproc` com folga; sem `RLIMIT_NPROC` global rebaixado.
- [ ] `RUNNER_SANDBOX=require` num deploy de verificação → confirmar `nsjail ENABLED`; depois `auto` em regime.
- [ ] Sem domínio público no runner; egress bloqueado (checklist do `RUNNER_SECURITY.md`).
- [ ] `runner-smoke` e `runner-security` verdes na pipeline.
- [ ] Corpus de escape (§4.4) contido no ambiente real.
