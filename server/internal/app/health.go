package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/httpx"
)

// Pinger is the part of the pool that /health needs.
type Pinger interface {
	Ping(ctx context.Context) error
}

const healthTimeout = 2 * time.Second

// HealthAPI implements GET /health. It answers 200 only while the database answers a ping, so a
// pod with no database is not routed traffic.
type HealthAPI struct {
	DB  Pinger
	Log *slog.Logger
}

// GetHealth implements the contract operation.
func (h *HealthAPI) GetHealth(ctx context.Context, _ api.GetHealthRequestObject) (api.GetHealthResponseObject, error) {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	if err := h.DB.Ping(ctx); err != nil {
		h.Log.WarnContext(ctx, "health: database ping failed", "request_id", httpx.RequestID(ctx), "error", err)
		return nil, httpx.NewError(http.StatusServiceUnavailable, "Database unavailable")
	}
	return api.GetHealth200JSONResponse{Status: api.Healthy, Database: "ok"}, nil
}
