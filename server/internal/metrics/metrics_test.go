package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/metrics"
)

func TestRenderIsTheContract(t *testing.T) {
	got := string(metrics.Render(metrics.Snapshot{
		Held: 7, Waiting: 1, Limits: metrics.Limits{Base: 6, Overflow: 2},
		Refusals: map[string]int64{"global_capacity": 3},
	}))
	for _, line := range []string{
		"# TYPE mainloop_task_slots_held gauge",
		"mainloop_task_slots_held 7",
		"# TYPE mainloop_task_slots_limit gauge",
		`mainloop_task_slots_limit{tier="base"} 6`,
		`mainloop_task_slots_limit{tier="overflow"} 2`,
		"# TYPE mainloop_task_slots_waiting gauge",
		"mainloop_task_slots_waiting 1",
		"# TYPE mainloop_task_capacity_refusals_total counter",
		`mainloop_task_capacity_refusals_total{reason="child_capacity"} 0`,
		`mainloop_task_capacity_refusals_total{reason="global_capacity"} 3`,
	} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("missing %q in\n%s", line, got)
		}
	}
}

func TestOnlyGetMetricsIsServed(t *testing.T) {
	h := &metrics.Handler{}
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/", http.StatusNotFound},
		{http.MethodGet, "/api/tasks", http.StatusNotFound},
		{http.MethodPost, "/metrics", http.StatusMethodNotAllowed},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.want)
		}
	}
}
