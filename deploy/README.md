# deploy/ — reproducible runner deploy

Turns the hand-pasted prod `docker run` (a dozen flags, easy to get wrong) into
one versioned script. Pairs with the narrative runbook in
[../docs/DEPLOY.md](../docs/DEPLOY.md) — that explains *why* each flag exists;
this *is* those flags, encoded.

## Files

| File | What |
|---|---|
| `deploy.sh` | the tool: `setup` / `up` / `verify` / `logs` / `token` |
| `runner.env.example` | copy to `runner.env` (gitignored) to override any default |
| `dalivim-cgroup.service` | systemd unit that recreates the delegated cgroup every boot |
| `.runner-token` | *(generated, gitignored, 0600)* the service token |

## First deploy on a fresh VPS

Do the host prep in [docs/DEPLOY.md §1–6](../docs/DEPLOY.md) first (Docker, userns
sysctls, build the image). Then:

```sh
cd deploy
sudo ./deploy.sh setup     # R6: check driver, install + enable the reboot-safe cgroup unit
./deploy.sh up             # (re)create the runner with all the validated flags
./deploy.sh verify         # execute + egress-contained + memory_exceeded, from inside
./deploy.sh token          # -> set this as RUNNER_SERVICE_TOKEN on the Railway backend
```

`setup` requires the **cgroupfs** docker driver (it tells you how to switch if not).
For local/maintenance use without R6, set `CGROUP_PARENT=` and explicitly set
`RUNNER_CGROUP=auto` in `runner.env`, then skip `setup`. Keep that instance out of
production traffic: Go/JS/Java/TypeScript have no per-run RSS ceiling in this mode.

## Redeploy / upgrade

```sh
cd ~/dalivim-runner && git pull && docker build --network=host -t dalivim-runner .
cd deploy && ./deploy.sh up && ./deploy.sh verify
```

`up` reuses the running container's token automatically, so an upgrade never
breaks the backend's `X-Runner-Token`. Re-running is idempotent.

## The token, handled safely

`up` resolves the service token in this order and always saves it to
`.runner-token` (0600, gitignored):

1. `RUNNER_SERVICE_TOKEN` in `runner.env` (if you pin one), else
2. the **currently running** container's token — a safe cutover of a live runner, else
3. the saved `.runner-token` from a previous deploy, else
4. a freshly generated `openssl rand -hex 32`.

Whatever it resolves to must also be set as `RUNNER_SERVICE_TOKEN` on the backend
(`./deploy.sh token` prints it).

## Why `verify` runs inside the container

Many VPSes firewall the docker bridge (the reason the runner uses `--network host`),
so a host-side `curl` to a published port returns **empty** and makes a healthy
runner look broken. `verify` runs `docker exec … python3` against `127.0.0.1:8090`
inside the container, which never touches the bridge. It reads the token from the
container's own env, so it works against the `require`-mode prod runner.
