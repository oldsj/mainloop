-- Sweep exclusivity: one row per running reconcile sweep. Every statement is one short
-- transaction; no connection is held while the sweep runs. The lease length is measured on the
-- database clock.

-- Takes the sweep when no row exists or the row has lapsed. No row comes back when a live holder
-- has it (pgx.ErrNoRows); of racing takers, the row lock lets exactly one update it.
-- name: TakeSweepLease :one
INSERT INTO sweep_leases (name, holder, until)
VALUES ($1, $2, now() + make_interval(secs => @lease_seconds::float8))
ON CONFLICT (name) DO UPDATE SET holder = excluded.holder, until = excluded.until
WHERE sweep_leases.until <= now()
RETURNING until;

-- Pushes the holder's own lease. No row comes back when another holder took the sweep over.
-- name: ExtendSweepLease :one
UPDATE sweep_leases SET until = now() + make_interval(secs => @lease_seconds::float8)
WHERE name = $1 AND holder = $2
RETURNING until;

-- Releases the holder's own lease, if it still has it.
-- name: ReleaseSweepLease :exec
DELETE FROM sweep_leases WHERE name = $1 AND holder = $2;
