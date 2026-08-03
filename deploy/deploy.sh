#!/usr/bin/env bash
#
# Reproducible deploy for dalivim-runner. One command instead of a hand-pasted
# `docker run` with a dozen flags. See deploy/README.md.
#
#   ./deploy.sh setup         one-time host prep for R6 (cgroup delegation + reboot-safe unit)
#   ./deploy.sh up             (re)create the runner container from deploy/runner.env
#   ./deploy.sh verify         run the acceptance checks from INSIDE the container
#   ./deploy.sh verify-image   (opt-in, S3) cosign-verify a pulled GHCR image's signature + signer
#   ./deploy.sh logs           tail the runner logs
#   ./deploy.sh token          print the current service token
#
# The service token is never committed: `up` reuses the running container's token
# (safe cutover of a live runner), else the saved deploy/.runner-token, else a
# freshly generated one — and always persists it to deploy/.runner-token (0600).
#
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${ENV_FILE:-$HERE/runner.env}"
TOKEN_FILE="$HERE/.runner-token"

# --- defaults (override any of these in deploy/runner.env) --------------------
IMAGE=dalivim-runner
CONTAINER=runner
RUNNER_SANDBOX=require
RUNNER_NETWORK_ISOLATION=require
RUNNER_CGROUP=require                       # 'auto' until `setup` has made it reboot-safe
RUNNER_CGROUP_MOUNT=/sys/fs/cgroup/dalivim
CGROUP_PARENT=/dalivim                      # empty to disable R6 cgroup placement
RUNNER_STATIC_SECCOMP=""                    # F-D C/C++ run jail: off|complain|enforce (empty => off)
# NATIVE_TRACE=1 enables the Odin step-through tutor (B.2) by running the
# container with `--security-opt systempaths=unconfined`. It is OFF by default
# and it is a REAL trade-off, so read this before flipping it:
#
#   Why it is needed: the trace jail mounts a FRESH procfs (a debugger cannot
#   resolve a PIE binary's load base without /proc/<pid>/maps). Mounting procfs
#   inside a user namespace is refused by the kernel while the container's own
#   /proc is masked — Docker bind-mounts /dev/null over /proc/kcore and friends,
#   which makes it "not fully visible". Without this flag every mode=trace on
#   odin fails with: Failed to mount mandatory point: '/proc'.
#
#   What it costs: the CONTAINER's /proc stops being masked. The JAIL is
#   unaffected — student code still gets a fresh procfs of the jail's own PID
#   namespace, showing only the jail's handful of processes. So the exposure is
#   to something that has already escaped the jail, not to the student program.
#
#   If you do not run Odin activities with the step-through, leave it off.
NATIVE_TRACE=""
CPUS=1
MEMORY=1g
PIDS_LIMIT=512
NPROC=512
RUNNER_DEFAULT_MEMORY_MB=""                 # empty => the image's built-in default
RUNNER_MAX_MEMORY_MB=""

# --- S3 supply-chain verify (opt-in) -----------------------------------------
# Only used by `verify-image`, for the flow where you deploy by PULLING the signed
# GHCR image instead of building on target. The default deploy still builds from
# pinned source on the box (deploy/README.md), so these are inert unless you call
# `verify-image`. The signer identity is PINNED to this repo's publish workflow, so
# a signature from any other Fulcio identity is rejected — provenance must prove
# "built by us", not merely "signed by someone".
REGISTRY_IMAGE="${REGISTRY_IMAGE:-ghcr.io/jpierreribeiro/dalivim-runner}"
COSIGN_IDENTITY="${COSIGN_IDENTITY:-https://github.com/jpierreribeiro/dalivim-runner/.github/workflows/ci.yml@refs/heads/main}"
COSIGN_ISSUER="${COSIGN_ISSUER:-https://token.actions.githubusercontent.com}"

# shellcheck source=/dev/null
[ -f "$ENV_FILE" ] && . "$ENV_FILE"

c_blue=$'\033[1;34m'; c_red=$'\033[1;31m'; c_grn=$'\033[1;32m'; c_off=$'\033[0m'
log()  { printf '%s▶ %s%s\n' "$c_blue" "$*" "$c_off"; }
ok()   { printf '%s✓ %s%s\n' "$c_grn" "$*" "$c_off"; }
die()  { printf '%s✗ %s%s\n' "$c_red" "$*" "$c_off" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing dependency: $1"; }

resolve_token() {
  local t=""
  if [ -n "${RUNNER_SERVICE_TOKEN:-}" ]; then printf '%s' "$RUNNER_SERVICE_TOKEN" >"$TOKEN_FILE"
  elif t="$(docker exec "$CONTAINER" printenv RUNNER_SERVICE_TOKEN 2>/dev/null)" && [ -n "$t" ]; then
    printf '%s' "$t" >"$TOKEN_FILE"                       # reuse the LIVE container's token (cutover)
  elif [ -s "$TOKEN_FILE" ]; then :                       # reuse the saved token (redeploy)
  else need openssl; openssl rand -hex 32 | tr -d '\n' >"$TOKEN_FILE"; log "generated a new service token"; fi
  chmod 600 "$TOKEN_FILE"; cat "$TOKEN_FILE"
}

cmd_token() { [ -s "$TOKEN_FILE" ] && cat "$TOKEN_FILE" && echo || die "no token yet — run './deploy.sh up' first"; }
cmd_logs()  { docker logs -f "$CONTAINER"; }

# S3 supply-chain gate (opt-in). Prove a GHCR image digest was signed by THIS repo's
# publish workflow BEFORE trusting it — keyless, verifying the signer IDENTITY (not
# just "signed"), failing CLOSED on any mismatch, the same discipline as
# RUNNER_SANDBOX=require. For the pull-based deploy flow; run it before './deploy.sh
# up' against the pulled image. Usage: ./deploy.sh verify-image [ref] (default
# $REGISTRY_IMAGE:latest). cosign resolves the ref to a digest and checks the sig on it.
cmd_verify_image() {
  need cosign
  local ref="${1:-$REGISTRY_IMAGE:latest}"
  log "verifying signature + signer identity for $ref"
  cosign verify \
    --certificate-identity="$COSIGN_IDENTITY" \
    --certificate-oidc-issuer="$COSIGN_ISSUER" \
    "$ref" >/dev/null \
    || die "cosign verify FAILED for $ref — refusing to trust it (unsigned, tampered, or wrong signer). NOT deploying."
  ok "signature verified: $ref was signed by this repo's publish workflow ($COSIGN_IDENTITY)"
}

cmd_setup() {
  need docker
  [ "$(id -u)" = 0 ] || die "'setup' changes the host — run as root (sudo)"
  local drv; drv="$(docker info --format '{{.CgroupDriver}}')"
  log "docker cgroup driver: $drv"
  if [ "$drv" != cgroupfs ]; then
    printf '%s\n' \
      "R6 with --cgroup-parent=$CGROUP_PARENT needs the cgroupfs driver (this box: $drv)." \
      "On a single-purpose runner box, switch it and re-run setup:" \
      "  echo '{ \"exec-opts\": [\"native.cgroupdriver=cgroupfs\"] }' > /etc/docker/daemon.json" \
      "  systemctl restart docker" \
      "(systemd-driver hosts: delegate a dalivim.slice instead — see docs/DEPLOY.md §8b.)"
    die "cgroup driver is not cgroupfs"
  fi
  install -m 0644 "$HERE/dalivim-cgroup.service" /etc/systemd/system/dalivim-cgroup.service
  systemctl daemon-reload
  systemctl enable --now dalivim-cgroup.service
  ok "installed + enabled dalivim-cgroup.service (recreates the delegated cgroup every boot)"
  log "delegated subtree:"
  cat "$RUNNER_CGROUP_MOUNT/cgroup.subtree_control"
  ls -ld "$RUNNER_CGROUP_MOUNT"
}

cmd_up() {
  need docker
  # Guard the classic availability trap: require + no delegated cgroup = fail-closed boot.
  if [ "$RUNNER_CGROUP" = require ] && [ ! -d "$RUNNER_CGROUP_MOUNT" ]; then
    die "RUNNER_CGROUP=require but $RUNNER_CGROUP_MOUNT is absent — run './deploy.sh setup' first (or set RUNNER_CGROUP=auto in runner.env)"
  fi
  local token; token="$(resolve_token)"

  local cg=() ; [ -n "$CGROUP_PARENT" ] && cg=(
    --cgroup-parent="$CGROUP_PARENT" --cgroupns=host
    -v "$RUNNER_CGROUP_MOUNT:$RUNNER_CGROUP_MOUNT"
  )
  local lim=()
  [ -n "$RUNNER_DEFAULT_MEMORY_MB" ] && lim+=(-e "RUNNER_DEFAULT_MEMORY_MB=$RUNNER_DEFAULT_MEMORY_MB")
  [ -n "$RUNNER_MAX_MEMORY_MB" ]     && lim+=(-e "RUNNER_MAX_MEMORY_MB=$RUNNER_MAX_MEMORY_MB")
  [ -n "$RUNNER_STATIC_SECCOMP" ]    && lim+=(-e "RUNNER_STATIC_SECCOMP=$RUNNER_STATIC_SECCOMP")

  # Unmask the container's /proc only when the native step-through is enabled;
  # see NATIVE_TRACE above for exactly what that widens (and what it does not).
  local sysp=()
  if [ -n "$NATIVE_TRACE" ]; then
    sysp=(--security-opt systempaths=unconfined)
    log "native step-through ENABLED: /proc unmasked for the container (see NATIVE_TRACE in deploy.sh)"
  fi

  log "recreating '$CONTAINER' from image '$IMAGE'"
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  docker run -d --name "$CONTAINER" --restart unless-stopped \
    --network host \
    "${cg[@]}" \
    -e "RUNNER_SERVICE_TOKEN=$token" \
    -e "RUNNER_SANDBOX=$RUNNER_SANDBOX" \
    -e "RUNNER_NETWORK_ISOLATION=$RUNNER_NETWORK_ISOLATION" \
    -e "RUNNER_CGROUP=$RUNNER_CGROUP" \
    -e "RUNNER_CGROUP_MOUNT=$RUNNER_CGROUP_MOUNT" \
    "${lim[@]}" \
    "${sysp[@]}" \
    --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
    --pids-limit="$PIDS_LIMIT" --ulimit "nproc=$NPROC" --cpus="$CPUS" --memory="$MEMORY" \
    "$IMAGE" >/dev/null
  sleep 2

  log "boot:"
  docker logs "$CONTAINER" 2>&1 | grep -E "nsjail ENABLED|cgroup memory accounting ENABLED|static seccomp" \
    || die "runner did not report 'nsjail ENABLED' — inspect: docker logs $CONTAINER"
  ok "up. token in $TOKEN_FILE — set the SAME value as RUNNER_SERVICE_TOKEN on the backend."
}

cmd_verify() {
  need docker
  # Hit 127.0.0.1:8090 from INSIDE the container: the docker bridge is firewalled on
  # many VPSes, so a host-side published-port curl returns empty (looks broken, isn't).
  #
  # This is the ON-TARGET proof of the memory containment GitHub CI cannot assert:
  # the R6 VPS runs with a delegated cgroup (RUNNER_CGROUP=require + --cgroup-parent),
  # so memory.max is authoritative and the RLIMIT_AS-incompatible runtimes (Go, JS,
  # Java) are contained as memory_exceeded — a guarantee the cgroup=auto CI runner
  # cannot make. It first confirms the cgroup posture from /readyz, then bombs each
  # of the three and requires memory_exceeded, so a silent downgrade to rlimit-only
  # fails the acceptance loudly instead of passing a weaker guarantee.
  docker exec -i "$CONTAINER" python3 - <<'PY'
import os, sys, json, urllib.request
tok = os.environ.get("RUNNER_SERVICE_TOKEN", "")
hdr = {"content-type": "application/json"}
if tok: hdr["X-Runner-Token"] = tok

def call(tag, lang, src, want, timeout_ms=None):
    payload = {"language": lang, "source_code": src}
    if timeout_ms:
        payload["timeout_ms"] = timeout_ms
    req = urllib.request.Request("http://127.0.0.1:8090/run",
        data=json.dumps(payload).encode(), headers=hdr)
    body = urllib.request.urlopen(req, timeout=30).read().decode()
    good = ('"status":"%s"' % want) in body
    print(("  ok  " if good else "  FAIL") + f" {tag}: want {want} -> {body[:90]}")
    return good

print("health:", urllib.request.urlopen("http://127.0.0.1:8090/healthz").read().decode())

# The R6 payoff is the cgroup posture: confirm memory_accounting is cgroup-v2 before
# asserting the Go/JS/Java bombs, so a FAIL there is unambiguous (posture, not bomb).
ready = json.loads(urllib.request.urlopen("http://127.0.0.1:8090/readyz").read().decode())
mem_acct = ready.get("memory_accounting", "")
cgroup_on = mem_acct.startswith("cgroup")
print(f"readyz: backend={ready.get('backend')} memory_accounting={mem_acct}")
if not cgroup_on:
    print("  FAIL cgroup posture: memory_accounting is not cgroup-v2 — this deploy is")
    print("       rlimit-only, so Go/JS/Java memory bombs are NOT contained as")
    print("       memory_exceeded. Enable the delegated cgroup (RUNNER_CGROUP=require +")
    print("       --cgroup-parent — see docs/DEPLOY.md §8b) before trusting the R6 guarantee.")
    sys.exit(1)

# Memory bombs for the RLIMIT_AS-incompatible runtimes — allocate real pages the
# cgroup memory.max must stop, silently (a print loop would trip the output cap and
# mask the memory outcome). Each MUST come back memory_exceeded on the live cgroup.
GO_BOMB   = "package main\nfunc main(){ var k [][]byte; for { b:=make([]byte,64*1024*1024); for i:=0;i<len(b);i+=4096 { b[i]=1 }; k=append(k,b) } }\n"
JS_BOMB   = "const k=[];for(;;){k.push(Buffer.alloc(64*1024*1024,1));}\n"
JAVA_BOMB = ("import java.util.*;\npublic class Main { public static void main(String[] a){ "
            "List<byte[]> k=new ArrayList<>(); for(;;){ k.add(new byte[64*1024*1024]); } } }\n")

results = [
    call("execute",       "python",     "print(2+2)", "success"),
    call("egress",        "python",     "import socket;socket.setdefaulttimeout(3);socket.create_connection(('1.1.1.1',80))", "runtime_error"),
    call("mem-bomb py",   "python",     "b=bytearray(10**10)", "memory_exceeded"),
    # R6 on-target: the cases GitHub CI (cgroup=auto) cannot prove.
    call("mem-bomb go",   "go",         GO_BOMB,   "memory_exceeded", timeout_ms=8000),
    call("mem-bomb js",   "javascript", JS_BOMB,   "memory_exceeded", timeout_ms=8000),
    call("mem-bomb java", "java",       JAVA_BOMB, "memory_exceeded", timeout_ms=8000),
]
sys.exit(0 if all(results) else 1)
PY
  ok "acceptance passed (execute + egress-contained + memory_exceeded for python/go/js/java on the live cgroup)"
}

case "${1:-}" in
  setup)        cmd_setup ;;
  up)           cmd_up ;;
  verify)       cmd_verify ;;
  verify-image) shift; cmd_verify_image "$@" ;;
  logs)         cmd_logs ;;
  token)        cmd_token ;;
  *) printf 'usage: %s {setup|up|verify|verify-image|logs|token}\n' "${0##*/}"; exit 2 ;;
esac
