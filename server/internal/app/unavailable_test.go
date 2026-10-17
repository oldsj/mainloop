package app_test

import (
	"net/http"
	"testing"
)

// Each of these routes has a domain handler that needs a dependency this test does not wire.
// They answer 503, and the contract declares it: newHandler checks every response against it.
func TestUnconfiguredServicesAnswerTheDeclared503(t *testing.T) {
	h := newHandler(t, nil, up())
	const schedule = `{"request_id":"r1","agent":{"namespace":"kagent","name":"a1"},"config":{"schedule":"0 * * * *","prompt":"go"}}`
	const sid = "6f1f3a0e-2f7c-4c43-9a43-0d4e9d7b2f11"
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/workspaces", ""},
		{"POST", "/workspaces", `{"repo":"oldsj/mainloop"}`},
		{"GET", "/workspaces/w1", ""},
		{"DELETE", "/workspaces/w1", ""},
		{"GET", "/workspaces/w1/ports", ""},
		{"POST", "/workspaces/w1/refresh", ""},
		{"POST", "/workspaces/w1/resume", ""},
		{"POST", "/workspaces/w1/suspend", ""},
		{"GET", "/tasks", ""},
		{"POST", "/tasks", `{"request_id":"r1","title":"t","brief":"b","mode":"code","project_id":"p1"}`},
		{"GET", "/tasks/t1", ""},
		{"GET", "/task-operations/o1", ""},
		{"POST", "/tasks/t1/cancel", `{"request_id":"r1","expected_version":1,"expected_attempt_id":null}`},
		{"POST", "/tasks/t1/retry", `{"request_id":"r1","expected_version":1,"expected_attempt_id":null}`},
		{"POST", "/tasks/t1/reassign", `{"request_id":"r1","expected_version":1,"expected_attempt_id":null,"target_profile_id":"pp1"}`},
		{"GET", "/hitl/r1/merge/p1/details?section=files", ""},
		{"GET", "/schedules", ""},
		{"POST", "/schedules", schedule},
		{"GET", "/schedules/" + sid, ""},
		{"GET", "/schedules/" + sid + "/executions", ""},
		{"DELETE", "/schedules/" + sid, ""},
		{"POST", "/schedules/" + sid + "/trigger", `{"request_id":"r1"}`},
		{"PUT", "/schedules/" + sid, `{"schedule":"0 * * * *","prompt":"go"}`},
		{"GET", "/devloop/lease", ""},
		{"POST", "/devloop/lease/acquire", ""},
		{"POST", "/devloop/lease/release", ""},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			hdr := map[string]string{}
			if c.method == "PUT" {
				hdr["If-Match"] = `"` + sid + `"`
			}
			if r := send(h, c.method, c.path, c.body, hdr); r.status != http.StatusServiceUnavailable {
				t.Fatalf("got %d: %s", r.status, r.body)
			}
		})
	}
}
