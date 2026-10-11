package health_test

import (
	"context"
	"encoding/json"
	"fmt"
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

// The partial index excludes collected history. Even with more than a batch undiscovered, the
// ordered candidate walk reads at most one batch and does not scan the retained receipt history.
func TestDiscoveryWalksTheIndexOfDeliveriesWithoutAReceipt(t *testing.T) {
	e := newEnv(t)
	const tracked = 30000
	e.exec(t, `INSERT INTO deliveries (id, idempotency_key, session_id, state, task_id, kagent_session_id, created_at, health_tracked)
		SELECT 'bulk-'||g, 'bulk-key-'||g, 'session-1', 'completed', 'bulk-task-'||g, $1, now() - (g * interval '1 second'), true
		FROM generate_series(1, $2::int) g`, runtimeSession, tracked)
	e.exec(t, `INSERT INTO health_collection (owner_id, gateway, session_id, a2a_task_id, delivery_id, receipt_at, state)
		SELECT $1, $2, session_id, task_id, id, created_at, 'collected' FROM deliveries WHERE id LIKE 'bulk-%'`, owner, gateway)
	for _, backlog := range []int{2, 1002} {
		if backlog == 1002 {
			e.exec(t, `INSERT INTO deliveries (id, idempotency_key, session_id, state, task_id, kagent_session_id, created_at)
				SELECT 'fresh-'||g, 'fresh-key-'||g, 'session-1', 'completed', 'fresh-task-'||g, $1, now()
				FROM generate_series(1, 1000) g`, runtimeSession)
		}
		e.exec(t, `ANALYZE deliveries`)
		e.exec(t, `ANALYZE health_collection`)
		e.exec(t, `ANALYZE sessions`)
		e.exec(t, `ANALYZE task_attempts`)
		e.exec(t, `ANALYZE tasks`)
		t.Run(fmt.Sprintf("%d_untracked", backlog), func(t *testing.T) {
			checkDiscoveryPlan(t, e, tracked, backlog)
		})
	}
}

type discoveryPlanNode struct {
	NodeType     string              `json:"Node Type"`
	RelationName string              `json:"Relation Name"`
	IndexName    string              `json:"Index Name"`
	SubplanName  string              `json:"Subplan Name"`
	ActualRows   int                 `json:"Actual Rows"`
	ActualLoops  int                 `json:"Actual Loops"`
	Plans        []discoveryPlanNode `json:"Plans"`
}

func checkDiscoveryPlan(t *testing.T, e *env, tracked, backlog int) {
	t.Helper()
	const batch = 500
	tx, err := e.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var raw []byte
	if err := tx.QueryRow(context.Background(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+discoveryStatement(t), pgx.NamedArgs{
		"since": time.Now().Add(-30 * 24 * time.Hour), "owner_id": owner, "gateway": gateway, "max_rows": batch,
	}).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plan []struct{ Plan discoveryPlanNode }
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 {
		t.Fatalf("expected one discovery plan, got %d", len(plan))
	}
	summary, err := json.MarshalIndent(plan[0].Plan, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("discovery plan over %d tracked and %d untracked deliveries:\n%s", tracked, backlog, summary)
	var indexed, limited bool
	var walk func(discoveryPlanNode)
	walk = func(n discoveryPlanNode) {
		if n.IndexName == "deliveries_health" {
			indexed = true
			if n.ActualRows*n.ActualLoops > batch {
				t.Errorf("candidate index reads %d rows, batch is %d", n.ActualRows*n.ActualLoops, batch)
			}
		}
		if n.SubplanName == "CTE batch" && n.NodeType == "Limit" && n.ActualRows == min(batch, backlog) {
			limited = true
		}
		if n.RelationName == "deliveries" && n.NodeType == "Seq Scan" {
			t.Error("the plan scans the deliveries table")
		}
		if n.RelationName == "health_collection" && strings.Contains(n.NodeType, "Scan") {
			t.Error("discovery scans retained receipts")
		}
		for _, child := range n.Plans {
			walk(child)
		}
	}
	walk(plan[0].Plan)
	if !indexed || !limited {
		t.Errorf("expected a limited candidate walk using deliveries_health: indexed %t, limited %t", indexed, limited)
	}
	var inserted, marked int
	if err := tx.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM health_collection WHERE state = 'pending'),
		(SELECT count(*) FROM deliveries WHERE health_tracked)`).Scan(&inserted, &marked); err != nil {
		t.Fatal(err)
	}
	if want := min(batch, backlog); inserted != want || marked != tracked+want {
		t.Errorf("inserted %d and marked %d, want %d and %d", inserted, marked, want, tracked+want)
	}
}
