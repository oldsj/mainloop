# Mainloop server

Go backend for Mainloop. One binary, `cmd/mainloop`, with a subcommand per process role. The
wire contract is [`../api/openapi.yaml`](../api/openapi.yaml); request validation and routing
come from it. See [`../ROADMAP.md`](../ROADMAP.md) for direction.

| Command                                             | Status                                                            |
| --------------------------------------------------- | ----------------------------------------------------------------- |
| `serve-api`                                         | Owner API on `:8000`. `/health` is real; other operations 501.    |
| `serve-mcp`                                         | `:8002`. `/health` only; 501 elsewhere.                           |
| `serve-git`                                         | `:8003` (read) and `:8004` (push). `/health` only; 501 elsewhere. |
| `migrate`, `sweep-orphans`, `smoke`, `kagent-probe` | Print "not implemented yet", exit 2.                              |

## Layout

- `internal/config`: the only package that reads the environment (a lint rule enforces it).
  Variable names match the Python backend's. `AGENT_TOKEN_KEY` is required.
- `internal/httpx`: request id, panic recovery, Host guard, foreign-Origin write guard, CORS, and
  the `{detail}` error shape.
- `internal/api`: code generated from the contract. Do not edit `*.gen.go`.
- `internal/app`: `Server` embeds one `*XAPI` per domain over a generated 501 fallback. To add a
  domain: write its `XAPI` with the contract's strict-server methods, add the field to `Server`,
  and add one line to `New`. Its methods win over the fallback. Each domain type needs a unique
  name (`sessions.SessionsAPI`, not `sessions.API`) or an alias in `app`: an embedded field takes
  its bare type name, so two `API` types collide.
- `internal/db`: the pgx pool (max 30 connections, `search_path=mainloop`).
- `tools/genfallback`: writes `internal/api/unimplemented.gen.go`.

## Run locally

Use a disposable PostgreSQL server (for example the test one). `/health` pings the database and
answers 503 when it is down; the server starts either way.

```sh
export AGENT_TOKEN_KEY=dev-only-key
export DEV_MODE=true                        # accept localhost Hosts and Origins
export DB_HOST=localhost DB_PORT=5432 DB_NAME=postgres DB_USER=mainloop DB_PASSWORD=...
go run ./cmd/mainloop serve-api
curl localhost:8000/health                  # {"database":"ok","status":"healthy"}
curl -i localhost:8000/attention            # 501 {"detail":"Not implemented"}
```

Without `DEV_MODE`, set `FRONTEND_DOMAIN` or `MAINLOOP_API_HOSTS` to the Host name you send.

## Test

```sh
export MAINLOOP_TEST_DATABASE_URL=postgresql://user:password@host:5432/postgres
go test -race ./...
golangci-lint run ./...
```

Database tests skip when `MAINLOOP_TEST_DATABASE_URL` is unset; CI always sets it.

## Regenerate

After `../api/openapi.yaml` changes:

```sh
go generate ./...
```

This runs `oapi-codegen` (pinned with the `tool` directive in `go.mod`) and then
`tools/genfallback`. CI fails if the committed output differs. `internal/api/overlay.yaml`
gives the contract's untyped `schema: {}` responses a raw-JSON Go type until the contract types
them; it stops matching on its own once they are typed.

## Image

```sh
docker build --target prod server    # debian-slim, git, util-linux, UID 1000, /usr/local/bin/mainloop
docker build --target dev server     # Go toolchain + watchexec
```
