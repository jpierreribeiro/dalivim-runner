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
cd dalivim-runner
# until F-B is merged to main, deploy the branch:
git checkout security-f05-nsjail
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
host↔container connectivity issues are catalogued in the
[README troubleshooting tables](../README.md#troubleshooting-runner_sandboxrequire-fails-to-boot).
The short version of the errors seen bringing this up, all fixed in the image:
kafel `umount` naming, dropped uid/gid mapping (no `newuidmap`), workdir on `/mnt`,
absolute interpreter path; plus the host-side `--security-opt`/`--network host`
and userns sysctls above.
