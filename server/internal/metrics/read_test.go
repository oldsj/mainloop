package metrics_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/metrics"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

// The endpoint reads the held, waiting and refused counts from the database.
func TestScrapeReadsTheDatabase(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO tasks (id, owner_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source, status)
		 VALUES ('root', 'u', 'root', 't', 'b', 'coordination', 'claude', 'explicit', 'running')`,
		`INSERT INTO tasks (id, owner_id, parent_task_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source, status, capacity_tier, runtime_wait_since)
		 VALUES ('c1', 'u', 'root', 'root', 't', 'b', 'coordination', 'claude', 'explicit', 'queued', 'base', NULL),
		        ('c2', 'u', 'root', 'root', 't', 'b', 'coordination', 'claude', 'explicit', 'queued', 'overflow', now())`,
		`INSERT INTO task_capacity_refusals (reason, total) VALUES ('child_capacity', 4)`,
	} {
		if _, err := d.Pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	h := &metrics.Handler{Pool: d.Pool, Limits: metrics.Limits{Base: 1, Overflow: 1}}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("%d %s\n%s", rec.Code, rec.Header().Get("Content-Type"), body)
	}
	for _, line := range []string{
		"mainloop_task_slots_held 2\n",
		"mainloop_task_slots_waiting 1\n",
		`mainloop_task_capacity_refusals_total{reason="child_capacity"} 4` + "\n",
		`mainloop_task_capacity_refusals_total{reason="global_capacity"} 0` + "\n",
	} {
		if !strings.Contains(body, line) {
			t.Errorf("missing %q in\n%s", line, body)
		}
	}
	if strings.Contains(body, "c1") || strings.Contains(body, "root") {
		t.Errorf("task ids in metrics:\n%s", body)
	}
}
