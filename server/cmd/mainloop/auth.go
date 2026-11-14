package main

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/oldsj/mainloop/server/internal/auth"
	"github.com/oldsj/mainloop/server/internal/config"
)

// newAuthenticator builds serve-api's authentication. ServiceAccount tokens are verified with a
// TokenReview, so only an in-cluster process that allows some ServiceAccount builds a reviewer.
func newAuthenticator(cfg *config.Config, log *slog.Logger) (*auth.Authenticator, error) {
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
	log.Info("auth configured", "mode", string(a.Mode), "owner_tokens", a.OIDC != nil,
		"service_accounts", a.ServiceAccounts != nil)
	return a, nil
}
