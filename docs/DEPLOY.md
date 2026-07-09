# Deploying dalivim-runner (root VPS)

The runner executes **untrusted student code** in an nsjail sandbox. Its isolation
depends on **unprivileged Linux user namespaces**, so it must run where those are
available: a **root VPS** (Hetzner, DigitalOcean, …), *not* a hardened managed
container PaaS (Railway/GuaraCloud/most Kubernetes), which typically blocks the
`clone(CLONE_NEWUSER)` nsjail needs.

This runbook is the exact sequence validated on Ubuntu 24.04 (2026-07-09):
`nsjail ENABLED`, `POST /run print(2+2)` → `4`, egress → `Network is unreachable`.

Architecture here: **backend on Railway → runner on a VPS**, two different clouds
with no private link, so the runner is reached over **public HTTPS**, gated by the
pre-shared `X-Runner-Token` and the sandbox.

```
[Railway backend] --HTTPS--> [Caddy :443 on VPS] --localhost--> [runner :8090] --per-run--> [nsjail cell]
       X-Runner-Token ------------------------------------------------^
```

---

## 1. Provision

- A small VPS (2 vCPU / 4 GB is plenty), **Ubuntu 24.04** or **Debian 12**.
- A DNS record (e.g. `runner.example.com`) pointing an `A` record at the VPS IP.

## 2. Install Docker

```sh
curl -fsSL https://get.docker.com | sh
```

## 3. Allow unprivileged user namespaces (host)

nsjail rootless needs these. Ubuntu 23.10+ restricts them via AppArmor by default.

```sh
sudo tee /etc/sysctl.d/99-userns.conf >/dev/null <<'EOF'
kernel.apparmor_restrict_unprivileged_userns=0
user.max_user_namespaces=15000
EOF
# Debian 12: replace the first line with  kernel.unprivileged_userns_clone=1
sudo sysctl --system
```

Verify (should print `ok`, no error):

```sh
unshare --user --net --map-root-user /bin/true && echo ok
```

## 4. Fix Docker's build DNS (only if `apt-get` fails during build)

On a fresh VPS the BuildKit sandbox often can't resolve DNS (`Temporary failure
resolving deb.debian.org`) because the host uses the systemd stub resolver.
Building with the host network sidesteps it:

```sh
# use this form in step 6:  docker build --network=host -t dalivim-runner .
```

(Optionally also set a daemon resolver: `echo '{"dns":["1.1.1.1","8.8.8.8"]}' |
sudo tee /etc/docker/daemon.json && sudo systemctl restart docker`.)

## 5. Get the code

```sh
git clone https://github.com/jpierreribeiro/dalivim-runner.git
cd dalivim-runner   # main has the nsjail sandbox (F-B)
```

## 6. Build

```sh
docker build --network=host -t dalivim-runner .
```

Multi-stage: static Go binary + nsjail compiled from source, on
`python:3.12-slim-bookworm`. The nsjail stage is cached after the first build, so
later rebuilds only recompile the Go binary.

## 7. Generate the token and run

```sh
# One strong secret, shared with the backend. Store it in your secret manager.
export TOKEN=$(openssl rand -hex 32); echo "SAVE THIS: $TOKEN"

docker run -d --name runner --restart unless-stopped \
  -e RUNNER_SERVICE_TOKEN=$TOKEN \
  -e RUNNER_SANDBOX=require \
  -e RUNNER_NETWORK_ISOLATION=require \
  --network host \
  --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
  --pids-limit=512 --ulimit nproc=512 --cpus=1 --memory=1g \
  dalivim-runner
```

Why each non-obvious flag:

| Flag | Why |
|---|---|
| `RUNNER_SANDBOX=require` | fail the boot closed if the jail can't engage — never run untrusted code unsandboxed |
| `--security-opt seccomp=unconfined` | Docker's default seccomp blocks `clone(CLONE_NEW*)` without `CAP_SYS_ADMIN`, which stops nsjail creating the namespaces. Loosens only the **outer supervisor** (trusted); the jail keeps its own seccomp on student code |
| `--security-opt apparmor=unconfined` | Ubuntu's docker-default AppArmor also restricts unprivileged userns |
| `--network host` | many VPSes firewall the docker bridge, so the published port resets (`curl` exit 56). Host networking skips the bridge. Per-run network isolation is unaffected — nsjail gives each run its own empty netns regardless |
| `--pids-limit / --ulimit nproc / --cpus / --memory` | outer container budget; the per-run caps are separate (`RUNNER_MAX_PROCESSES`, rlimits in the jail) |

**Never** use `--privileged` — nsjail does not need it and it would defeat the isolation.

## 8. Verify (the F-B acceptance check)

```sh
# a) the jail engaged
docker logs runner 2>&1 | grep "nsjail ENABLED"

# b) real execution in the cell
curl -s 127.0.0.1:8090/run -H "X-Runner-Token: $TOKEN" \
  -H 'content-type: application/json' \
  -d '{"language":"python","source_code":"print(2+2)"}'
#   -> {"status":"success","stdout":"4\n",...}

# c) egress is contained
curl -s 127.0.0.1:8090/run -H "X-Runner-Token: $TOKEN" \
  -H 'content-type: application/json' \
  -d '{"language":"python","source_code":"import socket;socket.setdefaulttimeout(3);socket.create_connection((\"1.1.1.1\",80))"}'
#   -> status runtime_error, "Network is unreachable"  (must NOT connect)
```

All three passing = the runner is executing untrusted code, contained.

## 8b. (Optional, recommended on a real VPS) Per-run cgroup accounting — R6

On a host where a cgroup v2 subtree can be **delegated** to the runner's uid, each
run gets its own `memory.max`/`pids.max` and `memory_exceeded` is read from the
kernel OOM event instead of a stderr substring — authoritative accounting that
also gives **Node** a hard memory ceiling `RLIMIT_AS` cannot. It needs cgroup v2
with the `memory`+`pids` controllers (Railway can't delegate these, a root VPS
can). It is **fail-safe**: without it the runner uses the rlimit/heap bound
exactly as before.

**Ship prod now with `RUNNER_CGROUP=auto`** (or leave it unset — `auto` is the
default). If no delegated cgroup is present it silently falls back to rlimits, and
your `--memory=1g` on the container is already a coarse OOM ceiling. Turning on the
full per-run accounting below is a maintenance-window task; don't block the launch
on it.

### The one gotcha: the runner's cgroup must live INSIDE the delegated subtree

`CLONE_INTO_CGROUP` (how the runner places nsjail into a run's cgroup) requires
write access to the **common ancestor** of the runner's own cgroup and the target
leaf. If the runner's cgroup is a *sibling* of the delegated subtree (the default —
its Docker scope lives under `system.slice`), the common ancestor is the **root**
`/sys/fs/cgroup` (root:root) and uid 1000 gets `permission denied` — a `chown` of
the leaf alone cannot fix it. The container must be **born inside** the subtree via
`--cgroup-parent`, so the common ancestor becomes the delegated (uid-1000-owned)
node. This also means the delegation approach depends on Docker's cgroup driver:

```sh
docker info --format '{{.CgroupDriver}}'    # cgroupfs  or  systemd
```

- **`cgroupfs`** (recipe below): a plain delegated directory + `--cgroup-parent`
  works. Simplest on a box dedicated to the runner.
- **`systemd`** (Ubuntu 24.04 default): systemd owns the hierarchy, so a hand-made
  `/sys/fs/cgroup/dalivim` fights it. Either switch the driver to `cgroupfs` (step 0
  below — fine for a single-purpose runner box) or delegate a real
  `dalivim.slice` with `Delegate=yes` and pass `--cgroup-parent=dalivim.slice`
  (more moving parts; ask and I'll write the unit).

**Rebuild the image from an R6 version first** (only retags `dalivim-runner`; the
live container is untouched until you redeploy it):

```sh
cd ~/dalivim-runner && git fetch origin && git pull   # or: git checkout <branch>
docker build -t dalivim-runner .                      # add --network=host if apt DNS fails
```

### Full enable (cgroupfs-driver route)

```sh
# 0) Use the cgroupfs driver (skip if `docker info` already shows cgroupfs).
#    NOTE: restarting docker restarts your containers — do it in a window.
echo '{ "exec-opts": ["native.cgroupdriver=cgroupfs"] }' | sudo tee /etc/docker/daemon.json
sudo systemctl restart docker
docker info --format '{{.CgroupDriver}}'              # -> cgroupfs

# 1) Delegate a writable subtree to the runner's in-container uid (1000).
sudo mkdir -p /sys/fs/cgroup/dalivim
echo "+memory +pids" | sudo tee /sys/fs/cgroup/dalivim/cgroup.subtree_control
sudo chown -R 1000:1000 /sys/fs/cgroup/dalivim

# 2) PROVE it end-to-end on a throwaway (NO --rm, so a fail-closed boot leaves
#    logs to read). The key flag is --cgroup-parent=/dalivim: it puts the
#    container's OWN cgroup under the delegated subtree, so the common ancestor
#    with the per-run leaves is /dalivim (uid 1000), and CLONE_INTO_CGROUP passes.
docker run -d --name runner-cgtest \
  --cgroup-parent=/dalivim \
  --cgroupns=host \
  -v /sys/fs/cgroup/dalivim:/sys/fs/cgroup/dalivim \
  -e RUNNER_ENV=development \
  -e RUNNER_SANDBOX=require \
  -e RUNNER_CGROUP=require \
  -e RUNNER_CGROUP_MOUNT=/sys/fs/cgroup/dalivim \
  --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
  --pids-limit=512 --cpus=1 --memory=1g \
  -p 8091:8090 dalivim-runner

sleep 2
docker logs runner-cgtest 2>&1 | tail -30     # want: nsjail ENABLED + cgroup memory accounting ENABLED
curl -s 127.0.0.1:8091/run -H 'content-type: application/json' \
  -d '{"language":"javascript","source_code":"const a=[];while(true){a.push(new Array(1e6).fill(7))}"}'; echo
#   -> {"status":"memory_exceeded",...}   (was runtime_error before R6)
docker rm -f runner-cgtest
```

If the container exits at boot, `docker logs` prints exactly why (with `require`
it fails closed) — a persisting `permission denied` on the clone means the
container is still outside the subtree (check `--cgroup-parent` and the driver).

### Cut the real runner over

Once the throwaway shows `cgroup memory accounting ENABLED` + `memory_exceeded`,
re-run the **section 7** `docker run` for the real `runner` with these additions.
Start with `RUNNER_CGROUP=auto` — it uses the cgroup when present and falls back to
rlimits otherwise, so it is safe even before persistence is set up:

```sh
  --cgroup-parent=/dalivim \
  --cgroupns=host \
  -v /sys/fs/cgroup/dalivim:/sys/fs/cgroup/dalivim \
  -e RUNNER_CGROUP=auto \
  -e RUNNER_CGROUP_MOUNT=/sys/fs/cgroup/dalivim \
```

Switch that `auto` to `require` **only after** the reboot-safe step below — with
`require`, a missing subtree fails the boot closed, which downs the runner on the
next reboot unless the subtree is recreated automatically.

### Make it reboot-safe — REQUIRED before you switch to `require`

> **`/sys/fs/cgroup` is tmpfs: the delegated subtree is gone after every reboot.**
> With `RUNNER_CGROUP=require` + `--restart unless-stopped` and no persistence, a
> reboot means: subtree gone → Docker restarts the runner → the R6 boot probe
> finds no delegated cgroup → **fail-closed → the runner does not come back up**
> until you re-create the subtree by hand. So `require` without the unit below is
> an availability time bomb, not hardening. Stay on `RUNNER_CGROUP=auto` until the
> subtree is recreated automatically on every boot.

Recreate the delegated subtree **before Docker starts**, with a systemd oneshot:

```ini
# /etc/systemd/system/dalivim-cgroup.service
[Unit]
Description=Delegate a cgroup v2 subtree to the dalivim runner (uid 1000)
Before=docker.service
After=sysinit.target
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/bin/mkdir -p /sys/fs/cgroup/dalivim
ExecStart=/bin/sh -c 'echo "+cpu +memory +pids" > /sys/fs/cgroup/dalivim/cgroup.subtree_control'
ExecStart=/usr/bin/chown -R 1000:1000 /sys/fs/cgroup/dalivim
[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload && sudo systemctl enable --now dalivim-cgroup.service
# prove it survives a reboot BEFORE trusting require:
sudo reboot                          # then, after it's back:
stat /sys/fs/cgroup/dalivim && ls -ld /sys/fs/cgroup/dalivim   # exists, owned by 1000
```

**Only after** the subtree is recreated automatically on boot, flip the real
runner to `-e RUNNER_CGROUP=require` — now the fail-closed behaviour is a feature
(it refuses to run untrusted code without the memory ceiling) rather than a
reboot trap, because the cgroup is guaranteed present.

The container runs as non-root uid 1000 with no added caps throughout.

## 9. Expose over HTTPS (Caddy) + firewall

The backend (Railway) reaches the runner over the public internet, so it must be
HTTPS and the raw `:8090` must **not** be public. Caddy gives automatic TLS.

```sh
sudo apt-get install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | sudo tee /etc/apt/sources.list.d/caddy-stable.list
sudo apt-get update && sudo apt-get install -y caddy
```

`/etc/caddy/Caddyfile`:

```
runner.example.com {
    reverse_proxy 127.0.0.1:8090
}
```

```sh
sudo systemctl reload caddy
```

Firewall — allow SSH + HTTP/HTTPS only; the runner's `:8090` stays private to the
box (Caddy reaches it over loopback):

```sh
sudo ufw allow 22,80,443/tcp
sudo ufw deny 8090/tcp        # block the raw runner port from the internet
sudo ufw enable
```

Verify from your laptop: `curl https://runner.example.com/healthz` → `ok`.

## 10. Point the backend at it (Railway)

Set on the **backend** service (it sends the token as `X-Runner-Token`):

```
RUNNER_SERVICE_URL=https://runner.example.com
RUNNER_SERVICE_TOKEN=<the same secret from step 7>
```

End-to-end smoke from anywhere with the token:

```sh
curl -s https://runner.example.com/run -H "X-Runner-Token: <secret>" \
  -H 'content-type: application/json' \
  -d '{"language":"python","source_code":"print(2+2)"}'
```

## 11. Operate

```sh
docker logs -f runner                 # tail logs
docker ps --filter name=runner        # status (Up vs Restarting)

# update to a new version
cd ~/dalivim-runner && git pull
docker build --network=host -t dalivim-runner .
docker rm -f runner && docker run -d --name runner --restart unless-stopped \
  -e RUNNER_SERVICE_TOKEN=$TOKEN -e RUNNER_SANDBOX=require \
  -e RUNNER_NETWORK_ISOLATION=require --network host \
  --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
  --pids-limit=512 --ulimit nproc=512 --cpus=1 --memory=1g dalivim-runner
```

Rotate the token by generating a new one, updating **both** the runner and the
backend, and recreating both. `--restart unless-stopped` brings the runner back
after a reboot (re-`export TOKEN` in new shells, or bake it into a `--env-file`).

## Troubleshooting

Boot probe errors (`RUNNER_SANDBOX=require but nsjail is unavailable: …`) and
host↔container connectivity issues are catalogued in the [README](../README.md).
The errors surfaced bringing the jail up on a real target are all fixed in the
image: kafel `umount` naming, rootless uid/gid self-map via `--user`/`--group`
(no `newuidmap`), the `/sandbox` mountpoint pre-created in the image, and an
absolute interpreter path (nsjail `execve`s with no PATH search). The host-side
pieces — `--security-opt seccomp=unconfined`/`apparmor=unconfined`,
`--network host`, and the userns sysctls — are covered in the steps above.
