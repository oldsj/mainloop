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

// maxConns bounds the pool. Nothing holds a connection across network I/O: reconcile sweeps keep
// their exclusivity in a lease row, not a pinned session lock. So demand is the number of queries
// in flight at once, not the number of sweeps and handlers running: 21 sweeps and about 10 lease
// loops (each with up to 4 handlers), plus HTTP and SSE, rarely have more than a dozen or so
// running a short statement together. 30 covers a burst with room to spare, and stays under a third of
// Postgres' default max_connections of 100 so two pods in a rolling update still fit. A query that
// cannot get a connection waits only until its own context ends, and sweep runs carry a deadline.
const maxConns = 30

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
