package httpx

import (
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recover turns a handler panic into a 500 {detail} and logs the panic with its stack. The
// panic value is not echoed to the client.
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if v == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
					panic(v)
				}
				log.ErrorContext(r.Context(), "panic in handler",
					"request_id", RequestID(r.Context()), "panic", v, "stack", string(debug.Stack()))
				WriteError(w, http.StatusInternalServerError, "Internal server error")
			}()
			next.ServeHTTP(w, r)
		})
	}
}
