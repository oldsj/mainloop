package devloop

import (
	"context"
	"net/http"
)

// HealthChecker polls another pod's own /health, the way the owner's dev-live doc already does
// with a port-forward and curl.
type HealthChecker interface {
	Healthy(ctx context.Context) (bool, error)
}

// HTTPHealthChecker is the real [HealthChecker]: one bounded GET.
type HTTPHealthChecker struct {
	// URL is the full health endpoint, e.g. the mainloop-backend Service's in-cluster DNS name.
	URL    string
	Client *http.Client
}

func (h *HTTPHealthChecker) Healthy(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL, nil)
	if err != nil {
		return false, err
	}
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK, nil
}
