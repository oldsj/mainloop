package main

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/app"
	"github.com/oldsj/mainloop/server/internal/auth"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/devices"
)

// newAuthenticator builds serve-api's authentication. ServiceAccount tokens are verified with a
// TokenReview, so only an in-cluster process that allows some ServiceAccount builds a reviewer.
// Device tokens are checked against the owner's registered devices in the database.
func newAuthenticator(cfg *config.Config, pool *pgxpool.Pool, log *slog.Logger) (*auth.Authenticator, error) {
	var reviewer auth.TokenReviewer
	if cfg.InCluster() && len(cfg.Auth.ServiceAccounts) > 0 {
		r, err := auth.NewInClusterReviewer()
		if err != nil {
			return nil, err
		}
		reviewer = r
	}
	a, err := auth.New(cfg, reviewer, &http.Client{Timeout: 5 * time.Second}, log)
	if err != nil {
		return nil, err
	}
	if pool != nil {
		a.Devices = &auth.Devices{Store: &devices.Store{Pool: pool, OwnerID: cfg.Identity.OwnerID}, MaxBody: app.MaxBodyBytes}
	}
	log.Info("auth configured", "mode", string(a.Mode), "owner_tokens", a.OIDC != nil,
		"service_accounts", a.ServiceAccounts != nil, "devices", a.Devices != nil,
		"device_registration", a.OIDC != nil && a.OIDC.DeviceAudience != "")
	return a, nil
}
