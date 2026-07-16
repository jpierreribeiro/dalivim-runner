#!/usr/bin/env bash
#
# smoke-escape.sh — adversarial corpus run against a LIVE, nsjail-backed runner.
#
# Every submission below MUST be contained by the per-run jail, and the runner
# itself MUST survive all of them (no daemon crash, no host OOM). The script
# fails (non-zero) the moment any containment expectation is not met, and ends
# with a host-survival check: a normal run must still succeed afterwards.
#
# G8.1 — the corpus is LANGUAGE-PARAMETRIZED. The core containment guarantees
# (no egress, no host write, no secret read, wall/CPU timeout) are asserted for
# EVERY runtime, so adding a language and proving it contained is one change:
# add the language to LANGS and give it a snippet per universal case. This
# enforces the ADDING-A-LANGUAGE rule structurally — "every new runtime must be
# re-proven contained" — instead of trusting scattered per-language CI steps.
#
# Case applicability (encoded in run_universal / the Python-only block):
#   egress, host-write, secret-read, cpu-spin  — ALL languages (kernel/mount/net
#       containment is language-agnostic, so we assert it for each runtime).
#   mem-bomb                                    — ASSERTED FOR EVERY LANGUAGE, but
#       HOW depends on the memory-bound posture, which this script PROBES from
#       /readyz (memory_accounting) instead of assuming:
#         * python/c/cpp/lua get a hard RLIMIT_AS, so a bomb is contained
#           DETERMINISTICALLY regardless of cgroup — asserted here in EVERY run.
#         * go/js/java/typescript/csharp opt OUT of RLIMIT_AS (they reserve a huge virtual cage a
#           tight RLIMIT_AS refuses) and are bounded by the delegated cgroup
#           memory.max / -Xmx instead. Their bomb is contained as memory_exceeded
#           ONLY when a delegated cgroup is engaged (memory_accounting=cgroup-v2).
#           When it is (deploy/deploy.sh verify on the R6 VPS, and the
#           runner-smoke-cgroup CI job), this script RUNS their bombs and asserts
#           memory_exceeded. When it is NOT (cgroup=auto — the default GitHub CI
#           runner-smoke, Railway, local dev), it does NOT silently skip: it prints
#           an EXPLICIT skip line naming the reason and where the case IS proven, so
#           the coverage gap is visible, never hidden.
#
#       COVERAGE MATRIX (mem-bomb):
#         python/c/cpp/lua/rust  RLIMIT_AS  contained everywhere (asserted every run)
#         go/js/java/ts/cs  cgroup memory.max memory_exceeded — asserted where cgroup
#                                           is engaged (VPS deploy verify +
#                                           runner-smoke-cgroup); explicitly skipped
#                                           (with a printed reason) where it is not.
#   fork bomb / env minimality / dangerous syscall — expressed once in Python:
#       they assert daemon/kernel-level controls (the --rlimit_nproc pids cap, the
#       minimal explicit env, the seccomp denylist SIGSYS) that are enforced by
#       the SAME jail for every language, so one expression proves the control.
#   SIGSYS-on-socket under the static allowlist — C/C++ only, and only meaningful
#       under RUNNER_STATIC_SECCOMP=enforce; asserted in the dedicated enforce
#       container in ci.yml, not here (this corpus runs on the default denylist).
#
# It deliberately does NOT weaken the jail — it only asserts the containment the
# jail already provides. See docs/RUNNER_ARCHITECTURE_AND_HARDENING_PLAN.md §4.4.
#
# Env:
#   RUNNER_URL             base URL of the runner   (default http://localhost:8090)
#   RUNNER_SERVICE_TOKEN   sent as X-Runner-Token   (optional; dev boot needs none)
#   ESCAPE_LANGS           space-separated language subset (default: all six)
set -euo pipefail

BASE="${RUNNER_URL:-http://localhost:8090}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# The full language set. Override with ESCAPE_LANGS to run a subset (e.g. when a
# toolchain is absent in a stripped image).
LANGS=(${ESCAPE_LANGS:-python javascript c cpp go java lua rust typescript csharp})

auth=()
if [ -n "${RUNNER_SERVICE_TOKEN:-}" ]; then
  auth=(-H "X-Runner-Token: ${RUNNER_SERVICE_TOKEN}")
fi

# post <language> <source> -> prints the raw JSON RunResult on stdout.
post() {
  local lang="$1" src="$2" body
  body="$(jq -nc --arg lang "$lang" --arg src "$src" '{language: $lang, source_code: $src}')"
  curl -fsS "${BASE}/run" \
    -H 'content-type: application/json' \
    "${auth[@]}" \
    -d "$body"
}

fail() { echo "ESCAPE FAIL [$1]: $2" >&2; echo "  resp: ${3:-<none>}" >&2; exit 1; }

# jqtrue <json> <filter> -> success iff the filter is truthy.
jqtrue() { echo "$1" | jq -e "$2" >/dev/null 2>&1; }

# ---------------------------------------------------------------------------
# Per-case, per-language source snippets. Each function echoes the source for
# the requested language; an unknown language aborts loudly so a newly-added
# runtime cannot silently skip a case.
# ---------------------------------------------------------------------------

# egress: probe an external IP and the cloud-metadata IP on :80. Prints
# "egress=..." and "metadata=..."; a reachable host prints OPEN (a failure).
src_egress() {
  case "$1" in
    python) cat <<'PY'
import socket
def probe(host, port):
    try:
        socket.create_connection((host, port), timeout=3).close()
        return "OPEN"
    except OSError as e:
        return "blocked:" + e.__class__.__name__
print("egress=" + probe("1.1.1.1", 80))
print("metadata=" + probe("169.254.169.254", 80))
PY
    ;;
    javascript) cat <<'JS'
const net = require("net");
function probe(host) {
  return new Promise((res) => {
    const s = net.connect({ host, port: 80 });
    s.setTimeout(3000);
    s.on("connect", () => { s.destroy(); res("OPEN"); });
    s.on("error", (e) => res("blocked:" + (e.code || "err")));
    s.on("timeout", () => { s.destroy(); res("blocked:timeout"); });
  });
}
(async () => {
  console.log("egress=" + await probe("1.1.1.1"));
  console.log("metadata=" + await probe("169.254.169.254"));
})();
JS
    ;;
    c|cpp) cat <<'C'
#include <stdio.h>
#include <string.h>
#include <unistd.h>
#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>
static const char *probe(const char *ip) {
  int fd = socket(AF_INET, SOCK_STREAM, 0);
  if (fd < 0) return "blocked:socket";
  struct sockaddr_in a;
  memset(&a, 0, sizeof a);
  a.sin_family = AF_INET;
  a.sin_port = htons(80);
  inet_pton(AF_INET, ip, &a.sin_addr);
  int rc = connect(fd, (struct sockaddr *)&a, sizeof a);
  close(fd);
  return rc == 0 ? "OPEN" : "blocked";
}
int main(void) {
  printf("egress=%s\n", probe("1.1.1.1"));
  printf("metadata=%s\n", probe("169.254.169.254"));
  return 0;
}
C
    ;;
    go) cat <<'GO'
package main

import (
	"fmt"
	"net"
	"time"
)

func probe(addr string) string {
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return "blocked"
	}
	c.Close()
	return "OPEN"
}

func main() {
	fmt.Println("egress=" + probe("1.1.1.1:80"))
	fmt.Println("metadata=" + probe("169.254.169.254:80"))
}
GO
    ;;
    rust) cat <<'RS'
use std::net::TcpStream;
use std::time::Duration;
fn probe(addr: &str) -> &'static str {
    match addr.parse().and_then(|a| Ok(TcpStream::connect_timeout(&a, Duration::from_secs(3)))) {
        Ok(Ok(_)) => "OPEN",
        _ => "blocked",
    }
}
fn main() {
    println!("egress={}", probe("1.1.1.1:80"));
    println!("metadata={}", probe("169.254.169.254:80"));
}
RS
    ;;
    typescript) cat <<'TS'
import * as net from "net";
function probe(host: string): Promise<string> {
  return new Promise((resolve) => {
    const s = net.connect({ host, port: 80 });
    s.setTimeout(3000);
    s.on("connect", () => { s.destroy(); resolve("OPEN"); });
    s.on("error", (e: NodeJS.ErrnoException) => resolve("blocked:" + (e.code ?? "err")));
    s.on("timeout", () => { s.destroy(); resolve("blocked:timeout"); });
  });
}
(async () => {
  console.log("egress=" + await probe("1.1.1.1"));
  console.log("metadata=" + await probe("169.254.169.254"));
})();
TS
    ;;
    csharp) cat <<'CS'
using System;
using System.Net.Sockets;
static string Probe(string host) {
    try { using var c = new TcpClient(); c.Connect(host, 80); return "OPEN"; }
    catch { return "blocked"; }
}
Console.WriteLine("egress=" + Probe("1.1.1.1"));
Console.WriteLine("metadata=" + Probe("169.254.169.254"));
CS
    ;;
    java) cat <<'JAVA'
import java.net.InetSocketAddress;
import java.net.Socket;

public class Main {
    static String probe(String host) {
        try (Socket s = new Socket()) {
            s.connect(new InetSocketAddress(host, 80), 3000);
            return "OPEN";
        } catch (Exception e) {
            return "blocked";
        }
    }
    public static void main(String[] a) {
        System.out.println("egress=" + probe("1.1.1.1"));
        System.out.println("metadata=" + probe("169.254.169.254"));
    }
}
JAVA
    ;;
    lua) cat <<'LUA'
-- Pure Lua 5.4 has no socket library — a submission cannot open a connection from
-- Lua itself. Its only network vector is os.execute; we attempt an outbound
-- connect through it and confirm nothing reaches the net (no OPEN). Containment is
-- layered: no native sockets, a minimal shell, and the empty netns behind it.
local function probe(host)
  local ok = os.execute("cat < /dev/tcp/" .. host .. "/80 >/dev/null 2>&1")
  return ok and "OPEN" or "blocked"
end
print("egress=" .. probe("1.1.1.1"))
print("metadata=" .. probe("169.254.169.254"))
LUA
    ;;
    *) echo "UNSUPPORTED_LANG:$1" >&2; return 1 ;;
  esac
}

# host-write: try to write outside the tmpfs /tmp. Prints "<path>=WRITTEN" on a
# successful write (a failure) or "<path>=denied".
src_hostwrite() {
  case "$1" in
    python) cat <<'PY'
out = []
for p in ("/usr/pwned", "/bin/pwned", "/app/pwned", "/sandbox/pwned"):
    try:
        with open(p, "w") as f:
            f.write("x")
        out.append(p + "=WRITTEN")
    except OSError:
        out.append(p + "=denied")
print(" ".join(out))
PY
    ;;
    javascript) cat <<'JS'
const fs = require("fs");
const out = [];
for (const p of ["/usr/pwned", "/bin/pwned", "/app/pwned", "/sandbox/pwned"]) {
  try { fs.writeFileSync(p, "x"); out.push(p + "=WRITTEN"); }
  catch (e) { out.push(p + "=denied"); }
}
console.log(out.join(" "));
JS
    ;;
    c|cpp) cat <<'C'
#include <stdio.h>
int main(void) {
  const char *ps[] = {"/usr/pwned", "/bin/pwned", "/app/pwned", "/sandbox/pwned"};
  for (int i = 0; i < 4; i++) {
    FILE *f = fopen(ps[i], "w");
    if (f) { fputs("x", f); fclose(f); printf("%s=WRITTEN ", ps[i]); }
    else { printf("%s=denied ", ps[i]); }
  }
  printf("\n");
  return 0;
}
C
    ;;
    go) cat <<'GO'
package main

import (
	"fmt"
	"os"
)

func main() {
	for _, p := range []string{"/usr/pwned", "/bin/pwned", "/app/pwned", "/sandbox/pwned"} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			fmt.Print(p + "=denied ")
		} else {
			fmt.Print(p + "=WRITTEN ")
		}
	}
	fmt.Println()
}
GO
    ;;
    rust) cat <<'RS'
use std::fs;
fn main() {
    for p in ["/usr/pwned", "/bin/pwned", "/app/pwned", "/sandbox/pwned"] {
        match fs::write(p, b"x") {
            Ok(_) => print!("{}=WRITTEN ", p),
            Err(_) => print!("{}=denied ", p),
        }
    }
    println!();
}
RS
    ;;
    typescript) cat <<'TS'
import { writeFileSync } from "fs";
const out: string[] = [];
for (const p of ["/usr/pwned", "/bin/pwned", "/app/pwned", "/sandbox/pwned"]) {
  try { writeFileSync(p, "x"); out.push(p + "=WRITTEN"); }
  catch { out.push(p + "=denied"); }
}
console.log(out.join(" "));
TS
    ;;
    csharp) cat <<'CS'
using System;
using System.IO;
var outp = new System.Collections.Generic.List<string>();
foreach (var p in new[] {"/usr/pwned", "/bin/pwned", "/app/pwned", "/sandbox/pwned"}) {
    try { File.WriteAllText(p, "x"); outp.Add(p + "=WRITTEN"); }
    catch { outp.Add(p + "=denied"); }
}
Console.WriteLine(string.Join(" ", outp));
CS
    ;;
    java) cat <<'JAVA'
import java.io.FileWriter;

public class Main {
    public static void main(String[] a) {
        String[] ps = {"/usr/pwned", "/bin/pwned", "/app/pwned", "/sandbox/pwned"};
        StringBuilder sb = new StringBuilder();
        for (String p : ps) {
            try (FileWriter w = new FileWriter(p)) { w.write("x"); sb.append(p).append("=WRITTEN "); }
            catch (Exception e) { sb.append(p).append("=denied "); }
        }
        System.out.println(sb.toString());
    }
}
JAVA
    ;;
    lua) cat <<'LUA'
local out = {}
for _, p in ipairs({"/usr/pwned", "/bin/pwned", "/app/pwned", "/sandbox/pwned"}) do
  local f = io.open(p, "w")
  if f then f:write("x"); f:close(); out[#out+1] = p .. "=WRITTEN"
  else out[#out+1] = p .. "=denied" end
end
print(table.concat(out, " "))
LUA
    ;;
    *) echo "UNSUPPORTED_LANG:$1" >&2; return 1 ;;
  esac
}

# secret-read: attempt to read /etc/shadow (owned by an uid unmapped in the
# userns). Prints "shadow=readable" (a failure) or "shadow=denied".
src_secret() {
  case "$1" in
    python) cat <<'PY'
shadow = "readable"
try:
    with open("/etc/shadow") as f:
        f.read()
except OSError:
    shadow = "denied"
print("shadow=" + shadow)
PY
    ;;
    javascript) cat <<'JS'
const fs = require("fs");
let shadow = "readable";
try { fs.readFileSync("/etc/shadow"); } catch (e) { shadow = "denied"; }
console.log("shadow=" + shadow);
JS
    ;;
    c|cpp) cat <<'C'
#include <stdio.h>
int main(void) {
  FILE *f = fopen("/etc/shadow", "r");
  printf("shadow=%s\n", f ? "readable" : "denied");
  if (f) fclose(f);
  return 0;
}
C
    ;;
    go) cat <<'GO'
package main

import (
	"fmt"
	"os"
)

func main() {
	s := "readable"
	if _, err := os.ReadFile("/etc/shadow"); err != nil {
		s = "denied"
	}
	fmt.Println("shadow=" + s)
}
GO
    ;;
    rust) cat <<'RS'
use std::fs;
fn main() {
    let s = if fs::read("/etc/shadow").is_ok() { "readable" } else { "denied" };
    println!("shadow={}", s);
}
RS
    ;;
    typescript) cat <<'TS'
import { readFileSync } from "fs";
let shadow = "readable";
try { readFileSync("/etc/shadow"); } catch { shadow = "denied"; }
console.log("shadow=" + shadow);
TS
    ;;
    csharp) cat <<'CS'
using System;
using System.IO;
string shadow;
try { File.ReadAllText("/etc/shadow"); shadow = "readable"; }
catch { shadow = "denied"; }
Console.WriteLine("shadow=" + shadow);
CS
    ;;
    java) cat <<'JAVA'
import java.nio.file.Files;
import java.nio.file.Paths;

public class Main {
    public static void main(String[] a) {
        String s = "readable";
        try { Files.readAllBytes(Paths.get("/etc/shadow")); } catch (Exception e) { s = "denied"; }
        System.out.println("shadow=" + s);
    }
}
JAVA
    ;;
    lua) cat <<'LUA'
local f = io.open("/etc/shadow", "r")
if f then f:read("a"); f:close(); print("shadow=readable")
else print("shadow=denied") end
LUA
    ;;
    *) echo "UNSUPPORTED_LANG:$1" >&2; return 1 ;;
  esac
}

# cpu-spin: an infinite loop the wall/CPU limit must stop (=> timeout).
src_spin() {
  case "$1" in
    python)     printf 'while True:\n    pass\n' ;;
    javascript) printf 'while (true) {}\n' ;;
    c|cpp)      printf 'int main(void) { for (;;) {} }\n' ;;
    go)         printf 'package main\n\nfunc main() { for {} }\n' ;;
    rust)       printf 'fn main() { loop {} }\n' ;;
    typescript) printf 'while (true) {}\n' ;;
    csharp)     printf 'while (true) {}\n' ;;
    java)       printf 'public class Main { public static void main(String[] a) { while (true) {} } }\n' ;;
    lua)        printf 'while true do end\n' ;;
    *) echo "UNSUPPORTED_LANG:$1" >&2; return 1 ;;
  esac
}

# mem-bomb: allocate far past the budget.
#
# python/c/cpp die under the hard RLIMIT_AS (python -> MemoryError/OOM; c/cpp ->
# malloc returns NULL and the program self-reports a non-zero exit). go/js/java opt
# out of RLIMIT_AS, so their bombs commit REAL pages the delegated cgroup
# memory.max (or the JVM -Xmx heap) must stop — they OOM only where a cgroup is
# engaged. Every bomb allocates SILENTLY (no per-iteration print): a print loop
# would trip the output cap first and mask the memory outcome we are asserting.
src_membomb() {
  case "$1" in
    python) printf 'b = b"x" * (10 ** 10)\nprint(len(b))\n' ;;
    c|cpp) cat <<'C'
#include <stdlib.h>
#include <string.h>
int main(void) {
  size_t chunk = 64UL * 1024 * 1024;
  for (;;) {
    char *p = malloc(chunk);
    if (!p) return 1;   /* RLIMIT_AS refused the mapping — contained */
    memset(p, 1, chunk);
  }
}
C
    ;;
    # Go: keep growing a slice of touched 64 MiB buffers. No RLIMIT_AS bounds Go,
    # so RSS climbs until the run's cgroup memory.max OOM-kills it (memory_exceeded).
    go) cat <<'GO'
package main

func main() {
	var keep [][]byte
	for {
		b := make([]byte, 64*1024*1024)
		for i := 0; i < len(b); i += 4096 {
			b[i] = 1 // commit the pages so RSS actually grows
		}
		keep = append(keep, b)
	}
}
GO
    ;;
    # Rust: grow a Vec of touched 64 MiB buffers until the hard RLIMIT_AS refuses the
    # allocation — Rust's allocator then aborts ("memory allocation of N bytes
    # failed"), contained like C (capAddressSpace:true), asserted every run.
    rust) cat <<'RS'
fn main() {
    let mut keep: Vec<Vec<u8>> = Vec::new();
    loop {
        let mut b = vec![0u8; 64 * 1024 * 1024];
        let mut i = 0;
        while i < b.len() { b[i] = 1; i += 4096; }
        keep.push(b);
    }
}
RS
    ;;
    # Node: OFF-HEAP Buffers, which --max-old-space-size does NOT bound — so RLIMIT_AS
    # and the V8 heap flag both miss them, and only the cgroup memory.max stops the
    # climb. Exactly the bound this case exists to prove.
    javascript) cat <<'JS'
const keep = [];
for (;;) {
  keep.push(Buffer.alloc(64 * 1024 * 1024, 1));
}
JS
    ;;
    # TypeScript compiles to JS and runs on the same Node jail — same off-heap Buffer
    # bomb, bounded by the cgroup memory.max (capAddressSpace:false), like javascript.
    typescript) cat <<'TS'
const keep: Buffer[] = [];
for (;;) {
  keep.push(Buffer.alloc(64 * 1024 * 1024, 1));
}
TS
    ;;
    # C#: retain touched 64 MiB byte[] chunks. The CLR opts out of RLIMIT_AS
    # (capAddressSpace:false), so RSS climbs until the cgroup memory.max OOM-kills it
    # (or the CLR throws OutOfMemoryException — the memErrSubstr fallback) — memory_exceeded.
    csharp) cat <<'CS'
using System.Collections.Generic;
var keep = new List<byte[]>();
while (true) {
    var b = new byte[64 * 1024 * 1024];
    for (int i = 0; i < b.Length; i += 4096) b[i] = 1;
    keep.Add(b);
}
CS
    ;;
    # Java: retain 64 MiB byte[] chunks past the -Xmx heap. The allocation exceeds
    # the heap (OutOfMemoryError, classified via the memErrSubstr fallback) and/or
    # the cgroup memory.max (OOM-kill) — either way memory_exceeded.
    java) cat <<'JAVA'
import java.util.ArrayList;
import java.util.List;

public class Main {
    public static void main(String[] a) {
        List<byte[]> keep = new ArrayList<>();
        for (;;) {
            keep.add(new byte[64 * 1024 * 1024]);
        }
    }
}
JAVA
    ;;
    # Lua: grow a table until the hard RLIMIT_AS refuses the allocation — PUC-Lua's
    # allocator raises "not enough memory" (a nonzero exit), contained deterministically
    # like python/c/cpp. Silent (no per-iteration print) so the output cap can't trip first.
    lua) printf 'local t = {}\nfor i = 1, 1e9 do t[i] = i end\nprint(#t)\n' ;;
    *) echo "UNSUPPORTED_LANG:$1" >&2; return 1 ;;
  esac
}

# ---------------------------------------------------------------------------
# Universal containment cases, asserted for every language in LANGS.
# ---------------------------------------------------------------------------
run_universal() {
  local lang="$1" resp

  echo "-- [$lang] egress + cloud metadata blocked (empty netns)"
  resp="$(post "$lang" "$(src_egress "$lang")" || true)"
  jqtrue "$resp" '.status == "success"'               || fail "egress/$lang" "run did not complete cleanly" "$resp"
  jqtrue "$resp" '.stdout | test("egress=blocked")'   || fail "egress/$lang" "outbound network was reachable from the jail" "$resp"
  jqtrue "$resp" '.stdout | test("metadata=blocked")' || fail "egress/$lang" "cloud metadata IP was reachable from the jail" "$resp"
  jqtrue "$resp" '.stdout | test("OPEN") | not'       || fail "egress/$lang" "a connection succeeded — netns egress not contained" "$resp"

  echo "-- [$lang] host writes denied (read-only rootfs)"
  resp="$(post "$lang" "$(src_hostwrite "$lang")" || true)"
  jqtrue "$resp" '.status == "success"'               || fail "hostwrite/$lang" "run did not complete cleanly" "$resp"
  jqtrue "$resp" '.stdout | test("WRITTEN") | not'    || fail "hostwrite/$lang" "a write outside tmpfs /tmp succeeded — rootfs not read-only" "$resp"

  echo "-- [$lang] secret read denied (/etc/shadow)"
  resp="$(post "$lang" "$(src_secret "$lang")" || true)"
  jqtrue "$resp" '.status == "success"'               || fail "secret/$lang" "run did not complete cleanly" "$resp"
  jqtrue "$resp" '.stdout | test("shadow=denied")'    || fail "secret/$lang" "/etc/shadow was READABLE inside the jail" "$resp"

  echo "-- [$lang] CPU spin stopped by the wall/CPU limit (=> timeout)"
  resp="$(post "$lang" "$(src_spin "$lang")" || true)"
  [ -n "$resp" ] || fail "spin/$lang" "runner did not respond — possible hang" "$resp"
  jqtrue "$resp" '.status == "timeout"'               || fail "spin/$lang" "CPU spin was not stopped by the timeout" "$resp"
}

# mem-bomb for the RLIMIT_AS languages (python/c/cpp): a bomb is contained
# DETERMINISTICALLY here (the hard address-space cap refuses the mapping), so we
# only assert it did NOT report success and the runner stayed up.
run_membomb() {
  local lang="$1" resp
  echo "-- [$lang] memory bomb contained (hard RLIMIT_AS)"
  resp="$(post "$lang" "$(src_membomb "$lang")" || true)"
  [ -n "$resp" ] || fail "membomb/$lang" "runner did not respond — possible host OOM" "$resp"
  jqtrue "$resp" '.status != "success"'               || fail "membomb/$lang" "a huge allocation reported success — not contained" "$resp"
}

# mem-bomb for the RLIMIT_AS-incompatible languages (go/js/java): these are bounded
# by the delegated cgroup memory.max (and Java's -Xmx), so a bomb is contained as
# memory_exceeded — the AUTHORITATIVE kernel-OOM classification — but ONLY when a
# cgroup is engaged. Asserts the exact status, not merely "not success", because
# that is the guarantee this case exists to prove. Never called unless
# memory_accounting is cgroup-v2.
run_membomb_cgroup() {
  local lang="$1" resp
  echo "-- [$lang] memory bomb => memory_exceeded (cgroup memory.max authoritative)"
  resp="$(post "$lang" "$(src_membomb "$lang")" || true)"
  [ -n "$resp" ] || fail "membomb/$lang" "runner did not respond — possible host OOM" "$resp"
  jqtrue "$resp" '.status == "memory_exceeded"'       || fail "membomb/$lang" "cgroup did not classify the bomb as memory_exceeded (status=$(echo "$resp" | jq -r '.status // "<none>"'))" "$resp"
}

# cgroup_engaged probes /readyz (unauthenticated) for the runtime memory-bound
# posture. True iff a delegated cgroup v2 subtree is giving each run an
# authoritative memory.max — the only condition under which the go/js/java bombs
# are contained as memory_exceeded. This is the honest, runtime-observed gate: the
# corpus asserts the strong case exactly when the mechanism it depends on is live,
# and says so explicitly when it is not.
cgroup_engaged() {
  local ready
  ready="$(curl -fsS "${BASE}/readyz" 2>/dev/null || true)"
  echo "$ready" | jq -e '(.memory_accounting // "") | test("^cgroup")' >/dev/null 2>&1
}

echo "== escape corpus (languages: ${LANGS[*]}) =="
for lang in "${LANGS[@]}"; do
  run_universal "$lang"
done

# Resolve the posture ONCE (a single /readyz probe) and drive every decision from it.
if cgroup_engaged; then
  CG_ENGAGED=1; CG_MODE="engaged (cgroup memory.max authoritative)"
else
  CG_ENGAGED=0; CG_MODE="rlimit-only (no delegated cgroup)"
fi
echo "== memory bombs — memory_accounting: $CG_MODE =="

deferred=()
for lang in "${LANGS[@]}"; do
  case " $lang " in
    # RLIMIT_AS languages: contained deterministically, every run. Rust uses the
    # system allocator (capAddressSpace:true), so a hard RLIMIT_AS refuses the
    # allocation and Rust aborts ("memory allocation of N bytes failed") — like C.
    " python "|" c "|" cpp "|" lua "|" rust ") run_membomb "$lang" ;;
    # RLIMIT_AS-incompatible languages: memory_exceeded ONLY under a live cgroup.
    # typescript runs on the Node jail (capAddressSpace:false), so it joins this group.
    # csharp runs on the CoreCLR (capAddressSpace:false), so it joins it too.
    " go "|" javascript "|" java "|" typescript "|" csharp ")
      if [ "$CG_ENGAGED" = 1 ]; then
        run_membomb_cgroup "$lang"
      else
        echo "-- [$lang] memory bomb — SKIPPED here (memory_accounting=rlimit-only)"
        echo "     $lang opts out of RLIMIT_AS (capAddressSpace:false), so its bomb is bounded"
        echo "     by the delegated cgroup memory.max / -Xmx — NOT engaged in this environment."
        echo "     PROVEN ON-TARGET where the cgroup is live: 'deploy/deploy.sh verify' on the"
        echo "     R6 VPS and the runner-smoke-cgroup CI job. (See this script's header matrix.)"
        deferred+=("$lang")
      fi
      ;;
  esac
done
if [ "${#deferred[@]}" -gt 0 ]; then
  echo "== NOTE: memory-bomb containment for [${deferred[*]}] is NOT asserted in THIS run"
  echo "         (rlimit-only posture). It is asserted where a delegated cgroup is engaged —"
  echo "         deploy/deploy.sh verify (R6 VPS) and the runner-smoke-cgroup CI job. No silent gap."
fi

# ---------------------------------------------------------------------------
# Kernel/daemon-level controls, expressed once (Python). These assert the SAME
# jail mechanisms every language runs under — the pids cap, the minimal env, and
# the seccomp denylist — so a single expression proves the control for all.
# ---------------------------------------------------------------------------
echo "== daemon/kernel controls (python-expressed) =="

# Fork bomb -> contained by --rlimit_nproc / container pids cap; host lives.
resp="$(post python $'import os\nwhile True:\n    os.fork()' || true)"
[ -n "$resp" ] || fail forkbomb "runner did not respond — daemon may have been taken down" "$resp"
jqtrue "$resp" '.status != "success"'                 || fail forkbomb "fork bomb reported success — not contained" "$resp"
echo "-- fork bomb contained (status=$(echo "$resp" | jq -r .status))"

# Host-env leak -> the child sees ONLY the minimal explicit env. With the G7
# determinism pin the runtime sets PATH + PYTHONUNBUFFERED + LANG/LC_ALL/TZ, and
# with G11 it also pins PYTHONHASHSEED; anything OUTSIDE this set would be a
# genuine host-env leak. (LC_ALL is now set, so CPython no longer PEP-538-coerces
# LC_CTYPE — it is not expected here.)
read -r -d '' SECRET <<'PY' || true
import os
allowed = {"PATH", "PYTHONUNBUFFERED", "LANG", "LC_ALL", "TZ", "PYTHONHASHSEED"}
leaked = sorted(k for k in os.environ if k not in allowed)
print("leaked=" + (",".join(leaked) or "none"))
print("tz=" + os.environ.get("TZ", "<unset>"))
PY
resp="$(post python "$SECRET" || true)"
jqtrue "$resp" '.status == "success"'                 || fail envleak "run did not complete cleanly" "$resp"
jqtrue "$resp" '.stdout | test("leaked=none")'        || fail envleak "host environment leaked into the jail" "$resp"
jqtrue "$resp" '.stdout | test("tz=UTC")'             || fail envleak "TZ was not pinned to UTC (determinism, G7)" "$resp"
echo "-- env minimal + determinism pinned (TZ=UTC)"

# Dangerous syscall -> killed by the nsjail seccomp denylist (SIGSYS).
read -r -d '' SYSCALL <<'PY' || true
import ctypes
libc = ctypes.CDLL(None, use_errno=True)
libc.unshare(0x10000000)  # CLONE_NEWUSER — on the seccomp KILL list
print("SURVIVED")
PY
resp="$(post python "$SYSCALL" || true)"
[ -n "$resp" ] || fail syscall "runner did not respond" "$resp"
jqtrue "$resp" '.status != "success"'                 || fail syscall "dangerous syscall was not blocked" "$resp"
jqtrue "$resp" '.stdout | test("SURVIVED") | not'     || fail syscall "process survived the killed syscall" "$resp"
echo "-- dangerous syscall killed by seccomp (status=$(echo "$resp" | jq -r .status))"

# Output flood -> captured stdout truncated at the cap, no runner OOM.
resp="$(post python $'for _ in range(10_000_000):\n    print("x" * 64)' || true)"
[ -n "$resp" ] || fail flood "runner did not respond — possible OOM/hang" "$resp"
jqtrue "$resp" '.stdout | test("\\[output truncated\\]")' || fail flood "output was not truncated at the cap" "$resp"
echo "-- output flood truncated, no OOM (status=$(echo "$resp" | jq -r .status))"

# ---------------------------------------------------------------------------
# Host-survival gate: after the whole corpus, a normal run must still succeed.
# ---------------------------------------------------------------------------
echo "== host survival =="
RUNNER_URL="$BASE" "$here/smoke-run.sh" python 'print(2+2)' $'4\n'
echo "ESCAPE CORPUS PASSED — every language contained, host survived."
