package main

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/apns"
	"github.com/oldsj/mainloop/server/internal/attention"
	"github.com/oldsj/mainloop/server/internal/config"
)

// newAPNs builds Apple push when its credentials are mounted, and nil when they are not
// (docs/specs/push-notifications.md). Credentials that are there but incomplete or wrong stop
// startup rather than leave the owner without push unnoticed.
func newAPNs(cfg *config.Config, pool *pgxpool.Pool, attn *attention.Service, log *slog.Logger) (*apns.Service, error) {
	if cfg.APNs.CredentialsDir == "" {
		return nil, nil
	}
	creds, err := apns.LoadCredentials(cfg.APNs.CredentialsDir)
	if errors.Is(err, apns.ErrNoCredentials) {
		log.Info("apns: off: no credentials mounted")
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("MAINLOOP_APNS_CREDENTIALS_DIR: %w", err)
	}
	log.Info("apns: on", "topic", cfg.APNs.Topic, "key_id", creds.KeyID)
	return &apns.Service{
		Pool: pool, OwnerID: cfg.Identity.OwnerID, Log: log, Attention: attn,
		Sender: &apns.Client{Creds: creds, Topic: cfg.APNs.Topic},
	}, nil
}
