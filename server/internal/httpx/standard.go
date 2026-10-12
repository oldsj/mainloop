package httpx

import (
	"log/slog"
	"net/http"
)

// Observed is the request id, access log and panic recovery of every listener, outermost first.
// The id is attached before the access line reads it, so no line has an empty request_id. The owner
// API uses it directly because preview hosts must be routed ahead of its Host guard; every other
// listener uses [Standard].
func Observed(log *slog.Logger) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		return Chain(h, RequestIDMiddleware, AccessLog(log), Recover(log))
	}
}

// Standard is the middleware every listener serves behind: [Observed], then the Host guard. Paths
// in exempt skip the Host check (probes address the pod by IP).
func Standard(log *slog.Logger, hosts map[string]bool, dev bool, exempt ...string) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		return Observed(log)(HostGuard(hosts, dev, exempt...)(h))
	}
}
