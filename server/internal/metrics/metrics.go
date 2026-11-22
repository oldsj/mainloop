// Package metrics serves Mainloop's Prometheus metrics: GET /metrics in the text exposition format,
// on a listener of its own (MAINLOOP_METRICS_ADDR) apart from the API and without authentication.
// It carries counts and configured limits only: no label names a task, an owner, a title or anything
// else a person wrote.
//
// Every value is read from the database at scrape time, so the numbers are the same whichever
// process admitted a task or refused one (serve-api and serve-mcp both admit):
//
//   - mainloop_task_slots_held: child tasks holding a global capacity slot, counted as admission
//     counts them, summed over owners
//   - mainloop_task_slots_limit{tier}: the base (TASK_MAX_CHILDREN_GLOBAL) and overflow
//     (TASK_MAX_CHILDREN_OVERFLOW) tiers of the global limit
//   - mainloop_task_slots_waiting: tasks given an overflow slot still waiting for runtime capacity
//   - mainloop_task_capacity_refusals_total{reason}: creates, retries, reassigns and follow-ups
//     refused for capacity, kept in the database, so it never resets
//
// See docs/specs/tasks.md#capacity.
package metrics

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// Refusal reasons the counter always reports, at 0 before the first refusal.
var refusalReasons = []string{dbstate.CapacityRefusalChild, dbstate.CapacityRefusalGlobal}

// scrapeTimeout bounds the database reads of one scrape.
const scrapeTimeout = 5 * time.Second

// Limits are the configured capacity limits the endpoint reports.
type Limits struct {
	Base     int
	Overflow int
}

// Snapshot is one reading of the metrics.
type Snapshot struct {
	Held     int64
	Waiting  int64
	Limits   Limits
	Refusals map[string]int64
}

// Handler serves /metrics from pool.
type Handler struct {
	Pool   *pgxpool.Pool
	Limits Limits
	Log    *slog.Logger
}

// Read takes a snapshot from the database.
func (h *Handler) Read(ctx context.Context) (Snapshot, error) {
	q := dbq.New(h.Pool)
	out := Snapshot{Limits: h.Limits, Refusals: map[string]int64{}}
	var err error
	if out.Held, err = q.CountAllGlobalCapacity(ctx); err != nil {
		return out, fmt.Errorf("count held slots: %w", err)
	}
	if out.Waiting, err = q.CountRuntimeWaiting(ctx); err != nil {
		return out, fmt.Errorf("count waiting tasks: %w", err)
	}
	rows, err := q.ListCapacityRefusals(ctx)
	if err != nil {
		return out, fmt.Errorf("read refusals: %w", err)
	}
	for _, r := range rows {
		out.Refusals[r.Reason] = r.Total
	}
	return out, nil
}

// ServeHTTP answers GET /metrics; anything else is not found.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/metrics" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), scrapeTimeout)
	defer cancel()
	s, err := h.Read(ctx)
	if err != nil {
		if h.Log != nil {
			h.Log.WarnContext(ctx, "metrics scrape failed", "error", err)
		}
		http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write(Render(s))
}

// Render writes a snapshot in the Prometheus text exposition format.
func Render(s Snapshot) []byte {
	var b bytes.Buffer
	gauge := func(name, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	}
	gauge("mainloop_task_slots_held", "Child tasks holding a global capacity slot, counted as admission counts them.")
	fmt.Fprintf(&b, "mainloop_task_slots_held %d\n", s.Held)
	gauge("mainloop_task_slots_limit", "Configured global child slots by tier: base is TASK_MAX_CHILDREN_GLOBAL, overflow is TASK_MAX_CHILDREN_OVERFLOW above it.")
	fmt.Fprintf(&b, "mainloop_task_slots_limit{tier=\"base\"} %d\n", s.Limits.Base)
	fmt.Fprintf(&b, "mainloop_task_slots_limit{tier=\"overflow\"} %d\n", s.Limits.Overflow)
	gauge("mainloop_task_slots_waiting", "Tasks given an overflow slot that are still waiting for runtime capacity.")
	fmt.Fprintf(&b, "mainloop_task_slots_waiting %d\n", s.Waiting)
	fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", "mainloop_task_capacity_refusals_total",
		"Task admissions refused for capacity, by reason.", "mainloop_task_capacity_refusals_total")
	for _, r := range refusalReasons {
		fmt.Fprintf(&b, "mainloop_task_capacity_refusals_total{reason=%q} %d\n", r, s.Refusals[r])
	}
	return b.Bytes()
}
