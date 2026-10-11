package health_test

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// discoveryStatement reads the sweep's discovery statement from the query file, so the plan checked
// here is the one that runs.
func discoveryStatement(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../db/queries/health.sql")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)-- name: DiscoverHealthReceipts :execrows\n(.*?);\n`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("DiscoverHealthReceipts is not in the query file")
	}
	return string(m[1])
}

// The discovery walk follows the deliveries that have no receipt yet: with a window full of
// deliveries that all have one, the plan reads the partial index (which holds only the few that do
// not) and never scans the deliveries table or sorts it.
func TestDiscoveryWalksTheIndexOfDeliveriesWithoutAReceipt(t *testing.T) {
	e := newEnv(t)
	const tracked = 30000
	e.exec(t, `INSERT INTO deliveries (id, idempotency_key, session_id, state, task_id, kagent_session_id, created_at, health_tracked)
		SELECT 'bulk-'||g, 'bulk-key-'||g, 'session-1', 'completed', 'bulk-task-'||g, $1, now() - (g * interval '1 second'), true
		FROM generate_series(1, $2::int) g`, runtimeSession, tracked)
	e.exec(t, `INSERT INTO health_collection (owner_id, gateway, session_id, a2a_task_id, delivery_id, receipt_at, state)
		SELECT $1, $2, session_id, task_id, id, created_at, 'collected' FROM deliveries WHERE id LIKE 'bulk-%'`, owner, gateway)
	e.exec(t, `ANALYZE deliveries`)
	e.exec(t, `ANALYZE health_collection`)

	tx, err := e.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	rows, err := tx.Query(context.Background(), "EXPLAIN (ANALYZE, BUFFERS) "+discoveryStatement(t), pgx.NamedArgs{
		"since": time.Now().Add(-30 * 24 * time.Hour), "owner_id": owner, "gateway": gateway, "max_rows": 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("discovery plan over %d tracked deliveries:\n%s", tracked, plan.String())
	if !strings.Contains(plan.String(), "deliveries_health") {
		t.Errorf("the plan does not use deliveries_health")
	}
	if strings.Contains(plan.String(), "Seq Scan on deliveries") {
		t.Errorf("the plan scans the deliveries table")
	}
	if strings.Contains(plan.String(), "Sort") {
		t.Errorf("the plan sorts")
	}
}
