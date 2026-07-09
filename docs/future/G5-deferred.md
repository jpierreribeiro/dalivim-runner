# G5 — Deferred / demand-gated

Real work, but **not worth doing until a concrete trigger fires**. Listed so the
triggers are written down and nobody builds them speculatively (YAGNI). Each entry
names the signal that should promote it out of this file.

---

## F-F — Per-run CPU-time accounting (cgroup `cpu.stat`)

**What**: report authoritative per-run CPU time (user+sys) from the cgroup's
`cpu.stat`, and optionally bound CPU *shares* per run, beyond today's
`--rlimit_cpu` wall-ish cap.

**Why deferred**: R6 already gives deterministic *memory* accounting and
`--rlimit_cpu` already kills CPU-bound loops. Per-run CPU *reporting* is a
nice-to-have for analytics, not a containment gap. The plan explicitly marked it
"not needed for R6".

**Promote when**: the backend wants to *display or grade on* CPU time (e.g. "your
solution used 1.2 s CPU"), or you need CPU-fairness between concurrent runs beyond
`--cpus`. Implementation locus: extend `runCgroup` (`cgroup_linux.go`) to read
`cpu.stat` after the run, like it reads `memory.events`/`memory.peak` today.

---

## Judge0 router / alternate execution backend

**What**: the `RunnerProvider` seam already lets the backend select a provider;
this would add a Judge0 (or other) backend behind it.

**Why deferred**: Judge0 needs a **privileged** container with cgroup-v1 control
(`isolate`) — it does **not** run on Railway, and the current nsjail runner on a
root VPS already covers the languages in scope. Adding it now is complexity with
no capability gain.

**Promote when**: you need a language the nsjail runner can't safely host, OR you
need managed multi-language scale you don't want to operate yourself, AND you have
a privileged host for it. Nothing to refactor first — the provider contract exists
(see `[[runner-provider-refactor]]`).

---

## Warm pool / pre-forked jails (cold-start latency)

**What**: keep a small pool of pre-initialised jails (or a pre-warmed JVM for
Java) to shave per-run setup latency under burst load.

**Why deferred**: per-run jail setup is already single-digit-to-low-tens of ms for
native languages; the concurrency limiter (8, 503 on overload) handles bursts by
shedding, not queueing. Premature until latency is measured and found wanting.

**Promote when**: G4 metrics show p99 latency dominated by jail/JVM *setup* (not
execution), or a class-submit spike makes 503 rates unacceptable. Warm JVMs
specifically become attractive once Java (G2.2) is live and its cold start proves
painful. Requires care: a reused jail must be provably clean between runs (a
warm-pool bug is a cross-submission isolation bug — high severity).

---

## ~~Per-language resource tuning~~ → promoted to [G6](G6-per-language-limits.md)

**Trigger fired**: G2.2 (Java) landed. The floor part is implemented —
`minTimeoutMs`/`minMemoryMB` on the specs, raised in the service layer, never
above the global ceilings; Java sets a 128 MB memory floor. Per-language
*defaults/ceilings* remain deferred — see G6's scope note for the (new) trigger.

---

## Notes
- Keep this file honest: when a trigger fires, **move** the item into a numbered
  `G*` spec and implement it — don't let deferred work rot here as an excuse.
- None of these are containment gaps. The security posture is complete; these are
  capability/scale/ergonomics.
