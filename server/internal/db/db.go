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

// DefaultMaxConns is the pool ceiling of every process but the API: serve-mcp, serve-git's read
// pool and its push pool.
const DefaultMaxConns = 10

// APIMaxConns is serve-api's pool ceiling. Nothing holds a connection across network I/O: reconcile
// sweeps keep their exclusivity in a lease row, not a pinned session lock. So demand is the number
// of queries in flight at once, not the number of sweeps and handlers running. serve-api runs about
// 21 sweeps and 10 lease loops (each with up to 4 handlers), plus HTTP and SSE; they rarely have
// more than a dozen or so running a short statement together, and 30 covers a burst. A query
// that cannot get a connection waits only until its own context ends, and sweep runs carry a
// deadline.
//
// Ceilings are per process and pools open lazily, so only a burst reaches them. A pod that runs all
// four pools (API 30, MCP 10, git read 10, git push 10) tops out at 60 connections, 120 during a
// rolling update, against Postgres' default max_connections of 100. Size Postgres for that, or
// lower the ceilings, if the processes share a pod.
const APIMaxConns = 30

// PoolOption adjusts a pool before it opens.
type PoolOption func(*pgxpool.Config)

// WithMaxConns sets the pool ceiling (default [DefaultMaxConns]).
func WithMaxConns(n int32) PoolOption { return func(c *pgxpool.Config) { c.MaxConns = n } }

// NewPool builds a pool from config. It does not connect: the first query does, so the server
// can start and report the database as down on /health instead of crash-looping.
func NewPool(ctx context.Context, cfg *config.Config, opts ...PoolOption) (*pgxpool.Pool, error) {
	return NewPoolFromURL(ctx, cfg.DatabaseURL(), opts...)
}

// NewPoolFromURL is [NewPool] for a connection string.
func NewPoolFromURL(ctx context.Context, url string, opts ...PoolOption) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(url)
	if err != nil {
		// pgx masks the password in its own errors.
		return nil, fmt.Errorf("db: parse connection settings: %w", err)
	}
	pc.MaxConns = DefaultMaxConns
	pc.ConnConfig.RuntimeParams["search_path"] = Schema
	for _, o := range opts {
		o(pc)
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}
	return pool, nil
}
