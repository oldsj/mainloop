# Development performance

Status: no measurements for the current stack yet. Python-era measurements are in git history before `e776537`.

## Open items

- Wall time of `go test -race ./...` and `golangci-lint run ./...` in `server/`, on the dev host and in the CI job.
- Wall time of the `web/` type check, lint, Vitest run and Vite build.

## Runtime notes (inspected, not measured)

- The `oldsj/kagent` main branch pins Substrate v0.3.0-alpha3, the version [architecture.md](architecture.md) documents. Its e2e notes say it runs up to 1,000 actors per worker by default. The same notes say agent ActorTemplates omit resource limits, so worker CPU and memory limits do not give a per-agent packing budget.
