#!/usr/bin/env bash
#
# Reproducible deploy for dalivim-runner. One command instead of a hand-pasted
# `docker run` with a dozen flags. See deploy/README.md.
#
#   ./deploy.sh setup     one-time host prep for R6 (cgroup delegation + reboot-safe unit)
#   ./deploy.sh up         (re)create the runner container from deploy/runner.env
#   ./deploy.sh verify     run the acceptance checks from INSIDE the container
#   ./deploy.sh logs       tail the runner logs
#   ./deploy.sh token      print the current service token
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
CPUS=1
MEMORY=1g
PIDS_LIMIT=512
NPROC=512
RUNNER_DEFAULT_MEMORY_MB=""                 # empty => the image's built-in default
RUNNER_MAX_MEMORY_MB=""

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
  docker exec -i "$CONTAINER" python3 - <<'PY'
import os, sys, json, urllib.request
tok = os.environ.get("RUNNER_SERVICE_TOKEN", "")
hdr = {"content-type": "application/json"}
if tok: hdr["X-Runner-Token"] = tok
def call(tag, src, want):
    req = urllib.request.Request("http://127.0.0.1:8090/run",
        data=json.dumps({"language": "python", "source_code": src}).encode(), headers=hdr)
    body = urllib.request.urlopen(req, timeout=20).read().decode()
    good = ('"status":"%s"' % want) in body
    print(("  ok  " if good else "  FAIL") + f" {tag}: want {want} -> {body[:80]}")
    return good
print("health:", urllib.request.urlopen("http://127.0.0.1:8090/healthz").read().decode())
results = [
    call("execute",   "print(2+2)", "success"),
    call("egress",    "import socket;socket.setdefaulttimeout(3);socket.create_connection(('1.1.1.1',80))", "runtime_error"),
    call("mem-bomb",  "b=bytearray(10**10)", "memory_exceeded"),
]
sys.exit(0 if all(results) else 1)
PY
  ok "acceptance passed (execute + egress-contained + memory_exceeded)"
}

case "${1:-}" in
  setup)  cmd_setup ;;
  up)     cmd_up ;;
  verify) cmd_verify ;;
  logs)   cmd_logs ;;
  token)  cmd_token ;;
  *) printf 'usage: %s {setup|up|verify|logs|token}\n' "${0##*/}"; exit 2 ;;
esac
