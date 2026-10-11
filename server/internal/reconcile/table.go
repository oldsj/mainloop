package reconcile

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrLeaseLost means the claim is no longer the row's current claim: the lease expired and
// another claimer took the row, or the row was settled already.
var ErrLeaseLost = errors.New("reconcile: lease lost")

// Querier is the part of a pool or transaction the claim operations need. *pgxpool.Pool and
// pgx.Tx both satisfy it, so an outcome can be recorded in the transaction that also writes the
// domain change and its event.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Table names a domain table of intent rows (see the package comment for its columns) and the
// states in which a row is up for work.
type Table struct {
	name string
	// states is the claimable states as a quoted SQL list. They are spliced in as literals, like the
	// name, so the planner can match the claim query to the partial claim index in any plan mode.
	states string
}

var identPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// NewTable describes an intent table. The name is unqualified (the search_path is the mainloop
// schema) and and each state must be a plain lower-case identifier; it panics otherwise, since both are
// spliced into SQL and a bad one is a programming error.
func NewTable(name string, claimable ...string) Table {
	if !identPattern.MatchString(name) {
		panic(fmt.Sprintf("reconcile: invalid table name %q", name))
	}
	if len(claimable) == 0 {
		panic("reconcile: a table needs at least one claimable state")
	}
	states := make([]string, len(claimable))
	for i, s := range claimable {
		if !identPattern.MatchString(s) {
			panic(fmt.Sprintf("reconcile: invalid claimable state %q", s))
		}
		states[i] = "'" + s + "'"
	}
	return Table{name: name, states: strings.Join(states, ", ")}
}

// Name is the table name.
func (t Table) Name() string { return t.name }

// Claim is one row a claimer holds a lease on.
type Claim struct {
	ID    string
	State string
	// Attempts counts claims of this row, this one included.
	Attempts   int
	Owner      string
	LeaseUntil time.Time
	table      Table
}

// Claim takes up to limit due rows, which are in a claimable state, have next_attempt_at in the
// past and carry no live lease. Concurrent claimers get disjoint rows. It commits before it
// returns: call out only afterwards.
func (t Table) Claim(ctx context.Context, q Querier, owner string, lease time.Duration, limit int) ([]Claim, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
UPDATE `+t.name+`
SET lease_owner = $1, lease_until = now() + make_interval(secs => $2::float8), attempts = attempts + 1
WHERE id IN (
    SELECT id FROM `+t.name+`
    WHERE state IN (`+t.states+`) AND next_attempt_at <= now()
      AND (lease_until IS NULL OR lease_until <= now())
    ORDER BY next_attempt_at, id
    LIMIT $3
    FOR UPDATE SKIP LOCKED)
  AND (lease_until IS NULL OR lease_until <= now())
RETURNING id, state, attempts, lease_until`,
		owner, lease.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("reconcile: claim %s: %w", t.name, err)
	}
	claims, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Claim, error) {
		c := Claim{Owner: owner, table: t}
		err := r.Scan(&c.ID, &c.State, &c.Attempts, &c.LeaseUntil)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile: claim %s: %w", t.name, err)
	}
	return claims, nil
}

// fenced is the WHERE clause that matches only the row's current claim. Domain queries that
// record an outcome together with other columns use the same predicate:
//
//	WHERE id = $1 AND lease_owner = $2 AND attempts = $3
const fenced = "id = $1 AND lease_owner = $2 AND attempts = $3"

func (c Claim) exec(ctx context.Context, q Querier, set string, args ...any) error {
	tag, err := q.Exec(ctx, "UPDATE "+c.table.name+" SET "+set+" WHERE "+fenced,
		append([]any{c.ID, c.Owner, c.Attempts}, args...)...)
	if err != nil {
		return fmt.Errorf("reconcile: update %s %s: %w", c.table.name, c.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s %s attempt %d", ErrLeaseLost, c.table.name, c.ID, c.Attempts)
	}
	return nil
}

// Finish moves the row to state and releases the lease.
func (c Claim) Finish(ctx context.Context, q Querier, state string) error {
	return c.exec(ctx, q, "state = $4, lease_owner = NULL, lease_until = NULL", state)
}

// RetryAfter keeps the row's state, releases the lease and makes the row due again after d.
func (c Claim) RetryAfter(ctx context.Context, q Querier, d time.Duration) error {
	return c.exec(ctx, q,
		"next_attempt_at = now() + make_interval(secs => $4::float8), lease_owner = NULL, lease_until = NULL",
		max(d, 0).Seconds())
}

// Extend pushes the lease to d from now and returns the new expiry.
func (c Claim) Extend(ctx context.Context, q Querier, d time.Duration) (time.Time, error) {
	rows, err := q.Query(ctx, "UPDATE "+c.table.name+
		" SET lease_until = now() + make_interval(secs => $4::float8) WHERE "+fenced+" RETURNING lease_until",
		c.ID, c.Owner, c.Attempts, d.Seconds())
	if err != nil {
		return time.Time{}, fmt.Errorf("reconcile: extend %s %s: %w", c.table.name, c.ID, err)
	}
	until, err := pgx.CollectOneRow(rows, pgx.RowTo[time.Time])
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, fmt.Errorf("%w: %s %s attempt %d", ErrLeaseLost, c.table.name, c.ID, c.Attempts)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("reconcile: extend %s %s: %w", c.table.name, c.ID, err)
	}
	return until, nil
}
