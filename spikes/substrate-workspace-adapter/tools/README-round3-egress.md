# Round 3 egress helpers

These source files are a live-only, dummy-credential harness for the 2026-09-23
Substrate preview run. They are not production credential-provider code.

- `round3-credprovider/main.go` implements a temporary mTLS gRPC credential
  provider and HTTPS echo endpoint. It reads only a Kubernetes Secret created
  specifically for this test. The echo endpoint compares the injected header
  to that dummy value and returns a boolean result; it never returns the value.
- `round3-egress-injection/main.go` updates one actor's egress policy to inject
  a credential for one hostname. `--prefix` supports bearer-token headers;
  omit it for the dummy echo check.
- `round3-claude-provider/main.go` is the separate Phase 3e preview provider.
  It is pinned to the Claude Secret URI and one actor SPIFFE ID, reads the
  credential from a read-only Secret mount, and logs only a success marker.
  It is test-run scaffolding, not a general-purpose or production provider.

Build from the pinned Substrate checkout (`cdac9baef81dd319b46086d695266e6161e9e592`),
where the imported internal packages and protobuf modules are available:

```sh
CGO_ENABLED=0 GOFLAGS=-mod=vendor go build -o /tmp/round3-credprovider ./cmd/round3-credprovider
CGO_ENABLED=0 GOFLAGS=-mod=vendor go build -o /tmp/round3-egress-injection ./cmd/round3-egress-injection
CGO_ENABLED=0 GOFLAGS=-mod=vendor go build -o /tmp/round3-claude-provider ./cmd/round3-claude-provider
```

Use only a throwaway dummy Secret with the first provider in a disposable
preview cluster. The Phase 3e provider is separately actor-bound; pass its
credential only as a read-only Secret mount. Never put a credential in source,
logs, actor commands, or echo responses.
