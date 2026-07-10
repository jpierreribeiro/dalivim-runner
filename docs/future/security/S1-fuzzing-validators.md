# S1 — Fuzz the request validators (path grammar, JSON decode, base64)

The runner has an excellent **example-based** test suite, but **no fuzzing**. The
highest-severity code in the repo — the attacker-controlled path validator that
gates a host filesystem write — is exactly the class of code where a fuzzer finds the
input the example tests never imagined. Go ships native fuzzing (`go test -fuzz`);
this is the lowest-effort, zero-deploy-risk hardening item on the roadmap.

---

## Motivation

`MaterializeFiles` calls the path validation *"the highest-severity step in the
codebase (the request path is attacker-controlled and the write happens on the host
before the jail execs), so it fails CLOSED at every stage"*
(`internal/executor/materialize.go:26-29`). It is defended by a grammar + per-component
rules + `path.Clean` idempotence + `os.Root`/`openat2` + `O_EXCL` + post-write
`lstat`. That is strong — and precisely why it deserves a fuzzer: the guarantee is
"**no** input escapes root", a universal claim that example tests can only sample.
Fuzzing turns a sampled claim into a searched one.

The parser surface is small, pure, and fast — ideal for fuzzing:

- **Path grammar / policy** — `validatePath` / `validatePaths`
  (`internal/executor/filepolicy.go:212-301`): the whole-path regex
  (`filepolicy.go:78`), the per-component regex (`:86`), the `path.Clean`-idempotence
  check (`:273-276`), depth/byte caps, extension/name allowlists, duplicate
  detection.
- **JSON request decode + coarse bounds** — `handler.decode`
  (`internal/transport/httpapi/handlers.go:96-167`): body-cap, the base64 decode of
  `stdin`/`stdins` (`:115-136`), the size checks.
- **Base64 stream encode/decode round-trip** — `encodeStream` /`wantBase64`
  (`internal/executor/encoding.go`).
- **Entrypoint resolution / Java class regex** — `resolveEntrypoint`
  (`filepolicy.go:307-326`), `javaClassRe` (`:93`).

## Current state

- Tests are entirely example-based: `filepolicy`/`materialize` have rich tables
  (`internal/executor/materialize_test.go`, and the path table exercised on-target in
  `.github/workflows/ci.yml:399-412`), but **no `Fuzz*` function exists anywhere** in
  the repo.
- The validators are already **pure functions of their input** (no I/O in
  `validatePath`; `MaterializeFiles` re-validates independently,
  `materialize.go:41-44`), so a fuzz target needs no fixtures or sandbox — it calls
  the function and asserts an **invariant**.
- CI runs `go test -race ./...` (`ci.yml:42-43`) but never `go test -fuzz`.

## Proposed change

Add **invariant-checking** fuzz targets next to the code they cover, plus a CI step
that runs a bounded fuzz budget on PRs and a longer one on the weekly schedule
(`ci.yml:14-21`). Fuzzers assert **properties**, not expected outputs:

### The load-bearing invariant (path validator)

```go
// FuzzValidatePath: no accepted path can escape the source root, ever.
func FuzzValidatePath(f *testing.F) {
    seeds := []string{"main.py", "src/util.c", "../etc/passwd", "/abs", "a/../b",
        "a//b", ".hidden", "-flag", "@argfile", "a\x00b", "café.py", "C:\\x"}
    for _, s := range seeds { f.Add(s) }
    p, _ := policyFor("python", testCaps)
    f.Fuzz(func(t *testing.T, raw string) {
        clean, err := validatePath(raw, p)
        if err != nil { return } // rejection is always fine — fail-closed
        // ACCEPTED ⇒ these must ALL hold, or containment is broken:
        if path.IsAbs(clean) { t.Fatalf("accepted absolute path %q", clean) }
        if clean != path.Clean(clean) { t.Fatalf("accepted non-normal %q", clean) }
        for _, c := range strings.Split(clean, "/") {
            if c == "." || c == ".." { t.Fatalf("accepted traversal %q", raw) }
        }
        // and the killer property: joined under a root, it stays under the root.
        root := "/job"
        if !strings.HasPrefix(filepath.Clean(filepath.Join(root, clean)), root+"/") {
            t.Fatalf("accepted path escapes root: %q -> %q", raw, clean)
        }
    })
}
```

### Additional targets

- **`FuzzDecode`** (transport): feed arbitrary bytes as the request body to
  `handler.decode`; invariant — it **never panics** and never returns a `req` whose
  post-decode `stdin`/`stdins` exceed the caps. (Uses `httptest` to build the
  `*http.Request`.)
- **`FuzzBase64RoundTrip`**: for any input, `encodeStream("base64", s)` decodes back
  to exactly `s` (round-trip identity), and text mode is the identity.
- **`FuzzResolveEntrypoint` / `FuzzJavaClass`**: an accepted Java entrypoint always
  matches `javaClassRe` and contains no `/` or `..`; an accepted path entrypoint is
  always one of the submitted files.
- **`FuzzMaterialize`** (integration, `t.TempDir()` root): for any small `files[]`,
  after `MaterializeFiles` **every created path is inside the root** (walk the tree,
  assert no symlink, no escape) or the call returned an error. This fuzzes the
  validator **and** the writer together — the end-to-end containment claim.

### CI wiring

- PR step: `go test -run=^$ -fuzz=Fuzz -fuzztime=60s ./internal/executor/...` (and the
  transport package) — a short budget catches regressions fast.
- Weekly (`schedule`, `ci.yml:14-21`): a longer `-fuzztime=15m` per target for deeper
  search, same cadence as the escape corpus.
- **Commit the corpus**: any crasher `go test` writes to `testdata/fuzz/` becomes a
  permanent regression seed (checked in), so a found bug never silently returns.

## Contract / config impact

None. Test-only. No runtime code changes (unless a fuzzer finds a bug — then the fix
is the win).

## Security considerations (of doing the hardening)

- **A fuzzer may find a real escape.** That is the point; treat any crasher as a
  security finding, fix fail-closed, and add the seed. Until triaged, a found crasher
  makes CI red — desired.
- **Keep fuzz targets pure/bounded** so they can't themselves become a resource sink
  in CI (the validators are pure and fast; `FuzzMaterialize` must cap `files[]`
  size/count so it doesn't write large trees — reuse the test caps).
- Fuzzing **cannot** prove the sandbox (that's the escape corpus, `smoke-escape.sh`);
  it proves the **pre-jail request validators**. Keep the two lanes distinct — S1 is
  the parser lane.

## Testing

The fuzz targets **are** the test. Additionally: a unit test that the committed
`testdata/fuzz` seeds all pass (so the corpus stays valid), and a CI assertion that
the fuzz step actually ran (non-zero executions) so a misconfigured `-run` filter
can't silently skip it.

## Effort

**S.** A handful of `Fuzz*` functions over already-pure functions + two CI steps. No
new dependencies (native Go fuzzing). Highest security ROI per hour on the roadmap.

## Phase S1 acceptance

- `Fuzz*` targets exist for the path validator, JSON decode, base64 round-trip, and
  entrypoint resolution, each asserting a **containment invariant** (not an expected
  output).
- CI runs a bounded fuzz budget on every PR and a longer one weekly; crashers are
  committed as regression seeds.
- Any escape the fuzzer finds is fixed fail-closed with a permanent seed.
