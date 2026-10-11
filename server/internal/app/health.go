package app

import (
	"context"
	"encoding/json"
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
// pod with no database is not routed traffic. The response is its own type so that it does not
// depend on how the contract types the 200 body.
type HealthAPI struct {
	DB Pinger
}

type healthOK struct{}

func (healthOK) VisitGetHealthResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	return json.NewEncoder(w).Encode(map[string]string{"status": "healthy", "database": "ok"})
}

// GetHealth implements the contract operation.
func (h *HealthAPI) GetHealth(ctx context.Context, _ api.GetHealthRequestObject) (api.GetHealthResponseObject, error) {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	if err := h.DB.Ping(ctx); err != nil {
		return nil, httpx.NewError(http.StatusServiceUnavailable, "Database unavailable")
	}
	return healthOK{}, nil
}
