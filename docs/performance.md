# Development performance

Status: no measurements for the current stack yet.

The earlier measurements covered the retired Python backend's test suite, its
`make check` run and its gVisor workspace attempts. They are in git history
before the Go/React cutover (commit `e776537`), and nothing here describes them.

## Open items for the Go server and React app

- Wall time of `go test -race ./...` and `golangci-lint run ./...` in `server/`, on the dev host and in the CI job.
- Wall time of the `web/` type check, lint, Vitest run and Vite build.

## Runtime notes (inspected, not measured here)

- The `oldsj/kagent` main branch pins Substrate v0.3.0-alpha3 (`go/go.mod`), the version [architecture.md](architecture.md) documents. Its e2e notes say v0.3.0-alpha3 runs up to 1,000 actors per worker by default (`--max-actors=1000`). The same notes say agent ActorTemplates currently omit resource limits, so worker CPU and memory limits do not give a per-agent packing budget. A kagent branch on v0.4.0-alpha1 exists in local work; it is not the supported pin and is not measured here.
- The gVisor binary defaults on the amd64 nightly are systrap, `overlay2=root:self`, `directfs=true`, `file-access=exclusive` and `file-access-mounts=shared`. This is flag evidence, not a measurement inside the arm64 sandbox.
- Filesystem and cache tuning (overlay, directfs, mount access, dentry cache) is proposed, not recommended, until workload measurements exist.
- Any deployment change goes through GitOps.
