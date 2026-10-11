package app

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/httpx"
)

// StubHandler is the handler of a listener whose slice has not landed: /health answers 200 and
// everything else 501. The Host guard applies, with loopback only in dev.
func StubHandler(cfg *config.Config, log *slog.Logger, allowedHosts map[string]bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		apierr.Write(w, apierr.NotImplemented)
	})
	return httpx.Chain(mux, httpx.Standard(log, allowedHosts, cfg.IsDev(), "/health"))
}
