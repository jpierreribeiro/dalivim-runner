# G15 — C# (.NET, VM-compiled, Shape C)

C# is the second VM-compiled language after Java (`ADDING-A-LANGUAGE.md:95` names it
next). It slots into **Shape C**: compile the source to IL, run the IL on a VM (the
CoreCLR, `dotnet`), reusing the generalised compiled runtime
(`internal/executor/compiled.go:264`) exactly as Java does — full-rootfs denylist
run jail, heap-flag + cgroup memory, a startup memory floor. The genuinely hard part
is not the shape (Java proved it) but keeping .NET **offline** — the toolchain wants
to restore NuGet packages, and the empty netns must never be something it depends on.

> **The line.** The runner compiles a single-file, framework-only C# program to an
> IL assembly and runs it on the CoreCLR in the same jail Java uses. It never
> restores a NuGet package, never touches the network — the shared framework
> assemblies shipped in the image are the whole surface.

---

## Motivation

C# is a top-tier enterprise/teaching language with no execution judge here. It is the
worked-example the guide anticipates for Shape C, so it reuses Java's runtime seam;
the value is a large language ecosystem for the cost of one (heavy) toolchain.

## Current state

- Shape C is landed: `javaSpec` (`compiled.go:197-233`) compiles with `javac`, runs
  `java … Main` via `runBin` in a **full-rootfs denylist** jail
  (`runFullRootfs:true`, `capAddressSpace:false`, `memErrSubstr:"OutOfMemoryError"`,
  `minMemoryMB:128`). The `compiledRuntime` two-jail flow and classification are
  reused unchanged.
- No `csharp`/`dotnet` entry exists in the registries, `filePolicies`
  (`filepolicy.go:112`), or `main.go` `NewService` (`cmd/runner/main.go:80-84`).
- The image ships a JDK but **no .NET** (`Dockerfile`).

## Proposed change

### Spec (`compiledLangSpec`)

```go
var csharpSpec = compiledLangSpec{
    name:       "csharp",
    sourceFile: "Main.cs",
    // Compile C# -> IL. Two SDK options (decide in implementation, both .NET SDK):
    //   (a) csc (Roslyn) direct: fastest, leanest — csc against the shared-framework
    //       ref assemblies -> Main.dll, plus a PINNED Main.runtimeconfig.json so
    //       `dotnet exec` can host it. No MSBuild, no restore.
    //   (b) `dotnet build -c Release --no-restore` over a baked framework-only
    //       template .csproj — more standard, but MSBuild startup is slower and it
    //       must be proven to need ZERO network restore (framework packs ship in the
    //       SDK). Prefer (a) for compile-budget headroom.
    // Whichever: OFFLINE is mandatory (DOTNET_* env below); a caller .csproj/nuget
    // config is forbidden by the file policy.
    compile:  []string{"csc", "-nologo", "-optimize+", "-out:{out}", "{src}"}, // sketch — (a)
    artifact: "Main.dll",
    // RUN on the CoreCLR. `dotnet exec` hosts the assembly; runBin is the dotnet
    // muxer. GC heap hard-limit is the -Xmx analogue (hex bytes from memory_mb),
    // authoritative bound is the cgroup.
    run:      []string{"exec", "{out}"},
    runBin:   []string{"dotnet"},
    runFullRootfs: true,    // CoreCLR is dynamically linked -> full rootfs (like the JVM)
    capAddressSpace: false, // the CLR reserves a large virtual space; RLIMIT_AS kills startup
    // Offline + quiet + writable HOME on the size-capped /tmp (the CLR writes
    // ~/.dotnet, temp). GCHeapHardLimit is set per-run from memory_mb ({mem}).
    runEnv:   determinismEnv(
        "DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_NOLOGO=1",
        "DOTNET_CLI_HOME=/tmp", "HOME=/tmp",
        "DOTNET_GCHeapHardLimit=0x{mem-hex}", // per-run, like -Xmx
    ),
    memErrSubstr: "OutOfMemoryException", // no-cgroup fallback OOM classify
    // CLR non-heap overhead (JIT, metadata) sits on top of the heap, like the JVM:
    // floor an undersized request so it starts at all.
    minMemoryMB:  128,
    // CLR cold start is sub-second but re-check on-target; the JVM needed no timeout
    // floor and C# is comparable — add minTimeoutMs only if a memory bomb races the
    // deadline before OOM (the Java reasoning, compiled.go:224-233).
    versionArgs:  []string{"--version"}, // `dotnet --version` -> "8.0.404"
    parseVersion: strings.TrimSpace,
}
```

Register in `main.go` `NewService` via `NewCSharp(sb, compiledConfig(cfg))`.

### Image (the weight decision)

Install the **.NET SDK, pinned** (`dotnet-sdk-8.0`, an LTS), from Microsoft's apt
feed or the pinned `dotnet-install` tarball copied into the image. It is **heavy
(~200–400 MB)** — the largest single toolchain here. A size follow-up (like Java's
JRE/JDK note, `ADDING-A-LANGUAGE.md:105`) is an SDK-for-compile / runtime-for-run
split, deferred. Pin the exact SDK version for reproducibility.

### The offline crux

.NET's default reflex is to **restore NuGet packages over the network**. For a
single-file, framework-only program this is unnecessary — the shared-framework
reference assemblies ship in the SDK. The spec must guarantee no restore ever runs:
`csc` direct (option a) sidesteps MSBuild/restore entirely; a `dotnet build` path
(option b) must pass `--no-restore` over a template that has **no** `PackageReference`
and be proven network-free on-target. The empty netns is the structural backstop,
but the runner must not merely rely on it — a build that *tries* to restore and
fails is a bad `compile_error`, not a clean run.

## Contract / config impact

- One new language id `csharp`; a `csharp` `FilePolicy` (`.cs`; forbid `.csproj`,
  `.sln`, `nuget.config`, `packages.config`, `bin`/`obj`). Multi-file (G3) is a
  follow-up; single-file `Main.cs` (top-level statements, C# 9+, or a `static Main`)
  first, with the same documented entrypoint convention as Java.
- No new limit knobs — reuses the compiled compile-timeout + the Java-style memory
  floor. `DOTNET_GCHeapHardLimit` derives from the existing `memory_mb`.

## Security considerations

- **Denylist only** (VM): the CLR needs `clone`/`openat`/JIT `mmap` — the static
  allowlist cannot apply. Before trusting it, `strace` a hello-world and confirm the
  CLR trips **none** of the denylist KILLs (`perf_event_open`/`ptrace`/`bpf`/module
  ops/`mount`) at startup — the exact check Java passed
  (`ADDING-A-LANGUAGE.md:131-134`).
- **Offline** (above): no restore, no telemetry (`DOTNET_CLI_TELEMETRY_OPTOUT=1`),
  no first-run experience (`DOTNET_NOLOGO=1`), a writable HOME confined to the
  size-capped `/tmp`.
- **Env hygiene**: like Java's `JAVA_TOOL_OPTIONS`, set the run env explicitly and
  never inherit the runner's — `DOTNET_*` and `COMPlus_*`/`DOTNET_gcServer` knobs must
  not leak in.
- **Memory**: `capAddressSpace:false` + `DOTNET_GCHeapHardLimit` + cgroup, proven at
  deploy time (cgroup engaged), like Go/Java — not the `cgroup=auto` CI path.
- **Determinism**: `Dictionary<,>` enumeration order is unspecified — a documented
  non-guarantee (use `SortedDictionary`/ordering), added to `docs/DETERMINISM.md`.

## Testing

- **Success**: `Console.WriteLine(2+2);` (top-level) → `success` / `"4\n"`; stdin echo
  (`Console.In.ReadToEnd`).
- **Compile error**: a type error → `compile_error`, `csc` diagnostics in
  `compile_output`, nothing executed.
- **Runtime error**: `throw new Exception("boom")` → `runtime_error`.
- **memory_exceeded** (deploy-time, cgroup + `GCHeapHardLimit`): an allocation bomb.
- **timeout** (`while(true){}`); **egress contained** (on-target, mandatory) via
  `System.Net.Sockets`/`HttpClient` failing closed; host write denied.
- **Offline proof**: the compile runs with the netns empty and completes without a
  restore attempt (no network dependency).

## Effort

**L.** Heaviest of the three: a large pinned SDK in the image, the offline-restore /
runtimeconfig plumbing (the real work), and the deploy-time memory proof. The runtime
*shape* is Java's, reused — the effort is image + offline hardening, not new sandbox
code.

## Phase G15 acceptance

- A `csharp` submission compiles to IL offline and runs on the CoreCLR in the
  full-rootfs denylist jail; `success`/`compile_error`/`runtime_error`/`timeout`/
  `memory_exceeded` classify correctly.
- No NuGet restore / network dependency; egress + host-write contained on-target; the
  CLR trips no denylist KILL.
- `runtime_version` reports the .NET version; `docs/DETERMINISM.md` records the
  `Dictionary` order non-guarantee.
