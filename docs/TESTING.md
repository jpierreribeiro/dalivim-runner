# Testing the runner

How to prove a deployed runner actually executes untrusted code **and contains it**.
Three checks, in `curl` and in Postman. Run them after [DEPLOY.md](DEPLOY.md).

The whole surface is tiny:

| Method & path | Auth | Purpose |
|---|---|---|
| `GET /healthz` | none | liveness — returns `ok` |
| `POST /run` | `X-Runner-Token` | execute code (language-agnostic) |
| `POST /run/python` | `X-Runner-Token` | **deprecated** alias, Python only |

`/run` request body ([`pkg/runnerapi`](../pkg/runnerapi)):

```json
{ "language": "python", "source_code": "print(2+2)", "stdin": "", "timeout_ms": 0, "memory_mb": 0 }
```

Only `language` + `source_code` are required; zero limits fall back to the service
default and are clamped to the hard ceiling (a language may also floor a limit —
Java lifts `memory_mb` below 128 to 128; see G6). Response:

```json
{ "status": "success", "stdout": "4\n", "stderr": "", "exit_code": 0,
  "duration_ms": 42, "memory_kb": 8192, "runtime_name": "python", "runtime_version": "3.12.x" }
```

`status` is one of `success | runtime_error | timeout | memory_exceeded |
compile_error | output_limit_exceeded | internal_error`. `output_limit_exceeded`
means the run flooded output past `RUNNER_MAX_OUTPUT_BYTES` and was killed for it
(partial output is still returned); `compile_error` is a compiled-language compile
failure with diagnostics in `compile_output`.

---

## The one gotcha: `unauthorized`

`POST /run` is a remote-code-execution surface, so every request must carry the
pre-shared secret as the **`X-Runner-Token`** header, compared constant-time. A
`401 unauthorized` almost always means the header value is wrong, not that the
runner is broken. The two ways it happens:

1. **You pasted the placeholder literally.** Docs write `<the-secret>` or
   `<64-hex>` as a *fill-me-in* marker. If you send `X-Runner-Token: <4ef3…b4c>`
   the runner sees the angle brackets as part of the value and it won't match.
   The real value has **no `< >`**.
2. **`$TOKEN` is empty in this shell.** `export TOKEN=…` does not survive a new
   terminal or a reboot, so `-H "X-Runner-Token: $TOKEN"` sends an empty header.

**Fix both at once — read the token straight from the running container** instead
of retyping it:

```sh
export TOKEN=$(docker exec runner printenv RUNNER_SERVICE_TOKEN)
echo "$TOKEN"      # sanity: 64 hex chars, no < >
```

---

## curl — the three acceptance checks

Run these **on the VPS** (or anywhere, swapping `127.0.0.1:8090` for your public
`https://<host>`). Set `$TOKEN` as above first.

```sh
# 0. liveness (no token)
curl -s 127.0.0.1:8090/healthz; echo
#   -> ok

# 1. real execution in the jail
curl -s 127.0.0.1:8090/run -H "X-Runner-Token: $TOKEN" \
  -H 'content-type: application/json' \
  -d '{"language":"python","source_code":"print(2+2)"}'; echo
#   -> {"status":"success","stdout":"4\n",...}

# 2. same thing over public HTTPS (proves Caddy + token end to end)
curl -s https://31-59-138-77.sslip.io/run -H "X-Runner-Token: $TOKEN" \
  -H 'content-type: application/json' \
  -d '{"language":"python","source_code":"print(2+2)"}'; echo
#   -> {"status":"success","stdout":"4\n",...}

# 3. CONTAINMENT — egress must be blocked
curl -s 127.0.0.1:8090/run -H "X-Runner-Token: $TOKEN" \
  -H 'content-type: application/json' \
  -d '{"language":"python","source_code":"import socket; socket.setdefaulttimeout(3); socket.create_connection((\"1.1.1.1\",80)); print(\"VAZOU\")"}'; echo
#   -> status "runtime_error", stderr mentions "Network is unreachable"
#   -> MUST NOT print "VAZOU" and MUST NOT be "success"
```

Passing all three = the runner executes untrusted code **and** the sandbox holds.
Extra sanity checks worth a look:

```sh
# jail actually engaged at boot
docker logs runner 2>&1 | grep "nsjail ENABLED"

# timeout path (default limit; expect status "timeout")
curl -s 127.0.0.1:8090/run -H "X-Runner-Token: $TOKEN" -H 'content-type: application/json' \
  -d '{"language":"python","source_code":"while True: pass"}'; echo

# wrong token is rejected (expect: unauthorized)
curl -s -o /dev/null -w '%{http_code}\n' 127.0.0.1:8090/run \
  -H 'X-Runner-Token: nope' -H 'content-type: application/json' \
  -d '{"language":"python","source_code":"print(1)"}'
```

---

## Postman

Yes — the runner is a plain HTTP service, so Postman tests it directly. A ready
collection lives in [`postman/`](../postman):

- `postman/dalivim-runner.postman_collection.json`
- `postman/dalivim-runner.postman_environment.json`

**Import & configure**

1. Postman → **Import** → drop both files. Pick the *Dalivim Runner* environment
   (top-right).
2. Set two variables:
   - `runnerUrl` → e.g. `https://31-59-138-77.sslip.io` (or `http://127.0.0.1:8090`
     if you tunnel/port-forward).
   - `runnerToken` → the value from `docker exec runner printenv RUNNER_SERVICE_TOKEN`.
     **Paste it raw — no `< >`.** (This is the same mistake as above.)

**Run the folder top to bottom** — each request has a test script asserting the
expected outcome, so a green run *is* the acceptance:

| Request | Asserts |
|---|---|
| `Health` | `200`, body `ok` |
| `Run — 2+2` | `200`, `status=success`, `stdout="4\n"` |
| `Run — stdin echo` | reads `stdin`, echoes it back |
| `Run — egress blocked ★` | `status=runtime_error`, **not** `success` (containment) |
| `Run — timeout` | `status=timeout` |
| `Auth — wrong token` | `401 unauthorized` (token gate works) |

The token is sent as the `X-Runner-Token` header at the collection level, so every
request inherits it; only `Auth — wrong token` overrides it on purpose.

> This collection hits the **runner** directly. To test the runner *through the
> backend* (`POST /api/student/sessions/:id/run`), use the backend's own Postman
> collection in `dalivim-backend/backend/postman/` — that exercises the full
> student → Railway → runner path.
