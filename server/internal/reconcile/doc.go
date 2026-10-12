// Package reconcile is the background-work framework: the domain ledgers are the queue, and loops
// drive intent rows to a final state. It has no job table of its own.
//
// # Intent rows
//
// A domain table that holds side effects to perform ("intents") follows one shape, so the
// framework can claim from any of them:
//
//	id               text PRIMARY KEY
//	idempotency_key  text NOT NULL UNIQUE   -- the remote side dedups on it
//	state            text NOT NULL          -- the domain's own state machine, text + CHECK
//	attempts         integer NOT NULL DEFAULT 0
//	next_attempt_at  timestamptz NOT NULL DEFAULT now()
//	lease_owner      text
//	lease_until      timestamptz
//
// Create the intent row (and any event) in the transaction that decides to do the work, then
// commit. A [Loop] claims it, calls out, and records the outcome in a new transaction. No
// transaction or lock is open while a handler runs, so a slow remote call never holds a database
// resource.
//
// # Claims and leases
//
// [Table.Claim] takes a batch with UPDATE ... WHERE id IN (SELECT ... FOR UPDATE SKIP LOCKED),
// stamps lease_owner and lease_until, and bumps attempts, in one statement that commits at once.
// While the lease is live no other claimer can take the row. If the claimer dies the lease
// expires and the next claim picks the row up again with attempts one higher. Outcomes are
// fenced on (lease_owner, attempts): a claimer that lost its lease cannot record anything
// ([ErrLeaseLost]). A handler that runs longer than the lease has it extended by a keepalive.
// The keepalive also fences the handler on local time: if it cannot confirm the lease (the database
// is unreachable, an extension stalls) before the last confirmed lease runs out, the handler's
// context is cancelled, so it stops before another claimer can take the row. Rows claimed as the
// process shuts down are released at once.
//
// # The claim index
//
// A claim scans for due rows in a non-final state. Give each intent table a partial index that
// holds only the claimable states, ordered the way claims read them, so finished rows (the vast
// majority over time) cost nothing:
//
//	CREATE INDEX <table>_claim ON <table> (next_attempt_at, id)
//	    WHERE state IN ('pending', 'retrying');   -- the non-final states
//
// The claim query lists the states as literals, so the planner can match it to the index under any
// plan mode (a bound parameter would defeat a generic plan). The index predicate must list the same
// states as the [Table]'s claimable states. The test table in reconcile_test.go carries a worked
// example.
//
// # Uncertain outcomes
//
// The framework retries only what the handler tells it to retry. A handler that cannot tell
// whether the remote side acted must observe the remote side before it sends again (look the
// object up by its idempotency key); it must not return [Retry] and replay blindly.
//
// # Wake-ups and sweeps
//
// A [Notifier] holds one LISTEN connection for the process and wakes loops by channel; a
// periodic poll is the backstop, so a lost notification only delays work. A [Sweep] is periodic
// housekeeping that must run in one place at a time: it runs under a lease row (sweep_leases) that it takes, extends
// while it works and deletes when it ends, and failing to take it means skip this round. No
// database connection is held while a sweep runs, so a slow dependency cannot exhaust the pool; each
// run also has a deadline. A holder that dies stops extending and another process takes over once
// the lease expires.
package reconcile
