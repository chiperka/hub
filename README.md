# Chiperka Hub

Self-hosted control panel for [Chiperka](https://about.chiperka.com). It is a small, single-binary alternative to Chiperka Cloud that you run on your laptop or on your own server.

- **Use the regular `chiperka` CLI.** `chiperka test --cloud` sends tests to the hub. Results stream back to your terminal, and JUnit/HTML reports and artifacts download as usual.
- **Attach `chiperka-worker`s from any machine with Docker.** Workers only make outbound HTTP(S) calls, so no inbound ports are needed.
- **Watch it all in the web UI.** It shows live runs, the queue and workers, and lets you drill into each test: assertions, HTTP and CLI exchanges, the step timeline, live logs and artifacts.

The hub speaks the same API that the CLI and worker already use with Chiperka Cloud, so neither needs any changes.
Everything is stored in SQLite plus plain files in a single data directory.

## Install

```bash
# Docker
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

### Embedded worker

The image bundles [chiperka-worker](https://github.com/chiperka/worker). To run tests on the same machine as the hub, enable it and mount the Docker socket:

```bash
docker run -d -p 8080:8080 -v chiperka-hub:/data -v /var/run/docker.sock:/var/run/docker.sock \
  -e CHIPERKA_LOCAL_WORKER=1 finie/chiperka-hub
```

When running the binary directly, put `chiperka-worker` on `PATH`, or point `CHIPERKA_WORKER_BIN` at it.

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
| `CHIPERKA_LOCAL_WORKER` | `0` | `1` = supervise an embedded `chiperka-worker` |
| `CHIPERKA_LOCAL_WORKER_CONCURRENCY`, `_CAPACITY`, `_MAX_CONTAINERS` | auto | Passed to the embedded worker |
| `CHIPERKA_WORKER_BIN` | `chiperka-worker` | Path to the worker binary |
| `CHIPERKA_JOB_TIMEOUT` | `300s` | Per-test timeout |
| `CHIPERKA_RETENTION_DAYS` | `30` | Delete finished runs older than this (`0` = keep forever) |

When running behind a reverse proxy, make sure it doesn't buffer `/api/events/*`, which is a server-sent events stream.

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
| `internal/localworker` | Embedded worker supervisor |
| `internal/model` | Test model shared with the CLI |
| `web/` | Vue 3 + Vite + Tailwind UI, embedded into the binary |

Releases are cut by pushing a semver tag (`1.2.3`). GitHub Actions then builds the binaries and the `finie/chiperka-hub` Docker image.

## License

Apache 2.0
