// Package db opens the PostgreSQL pool.
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/config"
)

// Schema is the PostgreSQL schema that holds Mainloop's tables.
const Schema = "mainloop"

// maxConns bounds the pool. The API is a single owner's control plane.
const maxConns = 10

// NewPool builds a pool from config. It does not connect: the first query does, so the server
// can start and report the database as down on /health instead of crash-looping.
func NewPool(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	return NewPoolFromURL(ctx, cfg.DatabaseURL())
}

// NewPoolFromURL is [NewPool] for a connection string.
func NewPoolFromURL(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(url)
	if err != nil {
		// pgx masks the password in its own errors.
		return nil, fmt.Errorf("db: parse connection settings: %w", err)
	}
	pc.MaxConns = maxConns
	pc.ConnConfig.RuntimeParams["search_path"] = Schema
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}
	return pool, nil
}
