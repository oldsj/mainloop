package db

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type txGuardKey struct{}

// WithTxGuard returns a context whose [InTx] transactions run guard first, inside the
// transaction, and roll back if it fails. The MCP server uses it to re-check the caller's
// credential under a row lock in every write a tool makes, so a revoke cannot slip in between the
// authentication and the write.
func WithTxGuard(ctx context.Context, guard func(ctx context.Context, tx pgx.Tx) error) context.Context {
	return context.WithValue(ctx, txGuardKey{}, guard)
}

// InTx runs fn in a transaction: commit when it returns nil, roll back otherwise. Never do
// network I/O inside fn; write an intent row, commit, then call out.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	guard, _ := ctx.Value(txGuardKey{}).(func(context.Context, pgx.Tx) error)
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if guard != nil {
			if err := guard(ctx, tx); err != nil {
				return err
			}
		}
		return fn(tx)
	})
}
