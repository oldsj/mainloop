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
- `internal/httpx`: request id, access log, panic recovery, Host guard, foreign-Origin write guard,
  CORS, and the `{detail}` error shape. `Chain` makes its first middleware the outermost.
  `httpx.Standard` (request id, access log, recovery, then the Host guard) fronts the MCP, git read
  and stub listeners, so every access line has a `request_id`; the owner API uses `httpx.Observed`
  (the same first three) because preview hosts are routed ahead of its Host guard. The push gate
  listener is the one exception: it logs no access line and never adopts a client's request id or
  logs a path, so it generates its own id and puts it in its one refusal line and the
  `x-mainloop-request-id` response header.
- `internal/api`: code generated from the contract. Do not edit `*.gen.go`.
- `internal/app`: `Server` embeds one `*XAPI` per domain over a generated 501 fallback. To add a
  domain: write its `XAPI` with the contract's strict-server methods, add the field to `Server`,
  and add one line to `New`. Its methods win over the fallback. Each domain type needs a unique
  name (`sessions.SessionsAPI`, not `sessions.API`) or an alias in `app`: an embedded field takes
  its bare type name, so two `API` types collide.
  `app.New` wires services the caller has already built: it builds no stand-in for a missing one.
  `cmd/mainloop` builds every service once per subcommand, each through its package's `NewService`
  (required collaborators are arguments, so a forgotten one fails to compile; options are only for
  collaborators a process may lack), and tests build `app.Deps` with `internal/app/apptest`.
  serve-git builds the push gate without a credential service because it never enrolls (the API
  process owns the credential Secrets); `pushgate.Service.Enroll` then returns
  `ErrEnrollUnavailable`.
- `internal/db`: the pgx pool (10 connections by default, 30 for serve-api, `search_path=mainloop`).
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
