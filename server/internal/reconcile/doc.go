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
// housekeeping that must run in one place at a time: it runs under a non-blocking advisory lock,
// and a loss means skip this round.
package reconcile
