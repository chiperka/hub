# Chiperka Hub

Self-hosted control panel for [Chiperka](https://about.chiperka.com). It is a small, single-binary alternative to Chiperka Cloud that you run on your laptop or on your own server.

- **Use the regular `chiperka` CLI.** `chiperka test --cloud` sends tests to the hub. Results stream back to your terminal, and JUnit/HTML reports and artifacts download as usual.
- **Attach `chiperka-worker`s from any machine with Docker.** Workers only make outbound HTTP(S) calls, so no inbound ports are needed.
- **Watch it all in the web UI.** It shows live runs, the queue and workers, and lets you drill into each test: assertions, HTTP and CLI exchanges, the step timeline, live logs and artifacts.

The hub speaks the same API that the CLI and worker already use with Chiperka Cloud, so neither needs any changes.
Everything is stored in SQLite plus plain files in a single data directory.

## Install

```bash
docker compose up -d   # hub + one worker, see docker-compose.yml

# or just the hub
docker run -d --name chiperka-hub -p 8080:8080 -v chiperka-hub:/data finie/chiperka-hub
# or grab a binary from the GitHub releases
```

Open http://localhost:8080. On first start you can create an admin account, or set `CHIPERKA_ADMIN_EMAIL` / `CHIPERKA_ADMIN_PASSWORD`.
For a private single-user setup on your own machine, you can switch login off with `CHIPERKA_AUTH=disabled`.

## Connect the CLI

Create an API token under **Connect CLI** in the UI, then:

```yaml
# .chiperka/chiperka.yaml
cloud:
  url: https://chiperka.example.com
  project: my-project        # created automatically on first run
```

```bash
export CHIPERKA_TOKEN=cpk_…   # or ./auth.json: {"chiperka-token": {"chiperka.example.com": "cpk_…"}}
chiperka test --cloud
```

`CHIPERKA_CLOUD_URL` overrides `cloud.url` without editing the file.

## Add workers

In **Workers → Add worker**, the hub creates a worker token and a ready-to-run command. Run it on any host with Docker:

```bash
docker run -d --name chiperka-worker --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e CHIPERKA_WORKER_TOKEN=spt_… \
  finie/chiperka-worker --api https://chiperka.example.com
```

### Join without the UI

To add workers from `docker compose up` or provisioning scripts, give the hub a join token. Then start any number of workers with that token:

```yaml
services:
  hub:
    image: finie/chiperka-hub
    environment:
      CHIPERKA_WORKER_JOIN_TOKEN: change-me   # any secret string
    ports: ["8080:8080"]
    volumes: ["chiperka-hub:/data"]

  worker:
    image: finie/chiperka-worker
    environment:
      CHIPERKA_API_URL: http://hub:8080
      CHIPERKA_WORKER_TOKEN: change-me         # the join token
      CHIPERKA_WORKER_NAME: worker-1           # optional, defaults to the hostname
    volumes: ["/var/run/docker.sock:/var/run/docker.sock"]

volumes:
  chiperka-hub:
```

A joining worker registers itself and receives its own worker token. If a worker restarts under the same name, it takes over its existing entry. Joined workers that stay offline for 24 hours are removed automatically. To stop new workers from joining, change or unset the join token. Workers that have already joined keep working until you delete them in the UI.

With `CHIPERKA_AUTH=disabled` and no join token set, a worker started without any token joins on its own.

Notes:

- Workers must be able to pull the service images your tests use.
- Run one worker per Docker daemon.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `CHIPERKA_ADDR` | `:8080` | Listen address |
| `CHIPERKA_DATA_DIR` | `./data` (`/data` in Docker) | Database, snapshots, logs, artifacts |
| `CHIPERKA_PUBLIC_URL` | derived from the request | External URL used in CLI/worker instructions |
| `CHIPERKA_AUTH` | `enabled` | `disabled` = no login and no API token needed (workers without a token may join unless a join token is set) |
| `CHIPERKA_ADMIN_EMAIL`, `CHIPERKA_ADMIN_PASSWORD` | – | Bootstrap admin when no user exists |
| `CHIPERKA_WORKER_JOIN_TOKEN` | – | Lets workers register themselves with this token (see [Join without the UI](#join-without-the-ui)) |
| `CHIPERKA_WORKER_IMAGE` | `finie/chiperka-worker:latest` | Worker image shown in "Add worker" commands |
| `CHIPERKA_JOB_TIMEOUT` | `300s` | Per-test timeout |
| `CHIPERKA_RETENTION_DAYS` | `30` | Delete finished runs older than this (`0` = keep forever) |
| `CHIPERKA_CLOUD_HETZNER_TOKEN` | – | Enables cloud workers on Hetzner (see [Cloud workers on demand](#cloud-workers-on-demand)) |
| `CHIPERKA_CLOUD_AWS_ACCESS_KEY_ID`, `CHIPERKA_CLOUD_AWS_SECRET_ACCESS_KEY`, `CHIPERKA_CLOUD_AWS_REGION` | –, –, `eu-central-1` | Enables cloud workers on AWS |

When running behind a reverse proxy, make sure it doesn't buffer `/api/events/*`, which is a server-sent events stream.

## Cloud workers on demand

For big runs the hub can rent servers from **Hetzner Cloud** or **AWS EC2**, spread the run across them and return them when the queue is empty. You pay only while tests run. Without provider credentials the hub behaves exactly as before.

```bash
CHIPERKA_CLOUD_HETZNER_TOKEN=…            # API token of a Hetzner project used only by Chiperka
CHIPERKA_CLOUD_AWS_ACCESS_KEY_ID=…        # IAM user limited by contrib/cloud-image/aws-iam-policy.json
CHIPERKA_CLOUD_AWS_SECRET_ACCESS_KEY=…
CHIPERKA_CLOUD_AWS_REGION=eu-central-1
```

Everything else is set in the UI under **Cloud → Settings**, at runtime: the provider, rules (“at least 300 tests waiting → 4 servers”), the server cap, the monthly budget, idle and lifetime limits, server types with fallbacks, an optional warm pool and the IP address SSH may come from. Changes apply within 5 seconds. Each one is logged and can be reverted. The **Cloud** page shows every server live: its state, how long each start-up step took, its tests, why it exists, why it is going away and what it costs.

**How servers connect.** The hub can stay behind a VPN with no inbound ports:

- The provider firewall lets SSH in only from the hub's public IP.
- The hub generates a host key for each server and pins it.
- The hub logs in with its own key (`DATA_DIR/cloud/hub_ed25519`). That key may only open one reverse tunnel: no shell and no other forwards.
- The worker on the server talks to the hub through that tunnel at `127.0.0.1:18080`.
- Each server's worker gets its own token and is named after the server.
- Test containers cannot reach the cloud metadata service.

**When servers start and stop:**

- **Start.** Servers start when the queue of pending tests stays above a rule for the scale-up delay. Servers that are already running count towards the rule. Small runs stay on your permanent workers.
- **Shutdown.** Once idle, the worker is paused, running tests finish (or are re-queued after a timeout), then the server is deleted.
  - **Hetzner** bills every started hour. An idle Hetzner server is kept until shortly before its paid hour ends, so the next run can use it for free.
  - **AWS** bills by the second. An AWS server goes after the idle time, and spot servers fall back to on-demand when capacity is missing.

**Safety nets against forgotten servers:**

- The hub deletes servers after idle time and after a maximum lifetime.
- It deletes servers whose worker never signs in.
- Every minute it reconciles with the provider and deletes managed servers it has no record of.
- It enforces a monthly budget.
- Every server powers itself off after 5 hours. AWS then terminates it.
- `chiperka-hub cloud janitor` deletes servers older than 5 hours even when the hub is down. Run it on a schedule.

**Server image.** Servers boot from a prepared image with Docker, the worker and your test images, so they start in about a minute. Without an image they install Docker on first boot, which is slower. Build and refresh the image on a schedule:

```bash
chiperka-hub cloud build-image --provider hetzner --image registry.example.com/app:master-php
```

`contrib/cloud-image/gitlab-ci.yml` has ready-made scheduled jobs for both commands.

On AWS the image's disk (`--disk-gb`, 30 GB by default) is the minimum disk of servers started from it: a smaller disk size in the settings is raised to it. Pass `--type` and `--location` when the default builder (`c7a.large` in `eu-central-1a`) is not available in your region or plan.

**Prices.** The hub asks the provider: Hetzner's API, the AWS Price List API for on-demand servers (`pricing:GetProducts`, part of the IAM policy) and EC2 for spot. When it cannot find out a server's price, it says "price unknown" instead of guessing, leaves the server out of the spend and asks again every minute. While such a server runs and a monthly budget is set, no new servers start, because the budget cannot be checked.

**Trying it locally.** `CHIPERKA_CLOUD_DEV_DOCKER=hetzner` (with `CHIPERKA_CLOUD_DEV_HUB_URL=http://host.docker.internal:8080`) makes every "server" a worker container on your local Docker daemon. You can try the rules, the UI and the lifecycle without credentials. Development only.

## How it works

```
chiperka CLI ── runs, snapshots, live events ──▶ ┌──────────────┐ ◀── browser
                                                 │ Chiperka Hub │
chiperka-worker ◀── claim, progress, results ──▶ │ SQLite+files │
```

- **Test compilation.** Each test is compiled into the job DAG that workers execute. The steps are services, setup, execution, teardown, hooks, assertions and artifact upload.
- **Queue.** Jobs are interleaved across runs, so one large run can't starve others. They are scheduled by the weight capacity each worker reports, and handed out via long-polling.
- **Recovery:**
  - Unacknowledged claims return to the queue.
  - Workers that stop sending heartbeats are marked offline, and their jobs are retried.
  - Infrastructure errors are retried up to twice.
  - Stopping a run cancels its jobs on the workers too.
- **Results API.** Results are also exposed at `/api/results/...`, in the same shape as the CLI's local result store.

### Results from the CLI and MCP

The results API serves remote runs (`cr-…`) and tests (`ct-…`) to `chiperka result` and the `chiperka mcp` tools.
The CLI side of this lives in [`contrib/cli-cloud-result-store.patch`](contrib/cli-cloud-result-store.patch) until it lands upstream.

## Development

```bash
make run     # build UI + server, start on :8080 without auth (data in ./data)
make dev     # Vite dev server with hot reload, proxies /api to :8080
make test    # go vet + integration tests (simulated CLI and worker over HTTP)
make docker  # build the Docker image
```

| Path | Contents |
|---|---|
| `main.go` | Configuration and wiring |
| `internal/store` | SQLite persistence |
| `internal/compile` | Test → job DAG compiler |
| `internal/server` | HTTP API, SSE, queue monitor, reports |
| `internal/cloud` | Cloud autoscaler: providers (Hetzner, AWS), SSH tunnels, settings, janitor, image builder |
| `internal/model` | Test model shared with the CLI |
| `web/` | Vue 3 + Vite + Tailwind UI, embedded into the binary |

Releases are cut by pushing a semver tag (`1.2.3`). GitHub Actions then builds the binaries and the `finie/chiperka-hub` Docker image.

## License

Apache 2.0
