package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/httpx"
)

// Pinger is the part of the pool that /health needs.
type Pinger interface {
	Ping(ctx context.Context) error
}

const healthTimeout = 2 * time.Second

// HealthAPI implements GET /health. It answers 200 only while the database answers a ping, so a
// pod with no database is not routed traffic. It also reports the build revision, so a deploy can
// be checked against the commit it should be running.
type HealthAPI struct {
	DB  Pinger
	Log *slog.Logger
	// Revision is the build's git commit, or "dev" for an unstamped build.
	Revision string
}

// GetHealth implements the contract operation.
func (h *HealthAPI) GetHealth(ctx context.Context, _ api.GetHealthRequestObject) (api.GetHealthResponseObject, error) {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	if err := h.DB.Ping(ctx); err != nil {
		h.Log.WarnContext(ctx, "health: database ping failed", "request_id", httpx.RequestID(ctx), "error", err)
		return nil, apierr.Unavailable("Database unavailable")
	}
	return api.GetHealth200JSONResponse{Status: api.Healthy, Database: "ok", Revision: &h.Revision}, nil
}
