package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/api"
)

// The validator fills the contract's body defaults into the request the handler decodes, so a
// default the service reads as the caller's choice changes what the request means.
func TestRequestValidatorLeavesAWorkspacesPortsToItsProject(t *testing.T) {
	t.Parallel()
	spec, err := api.GetSpec()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	h := requestValidator(spec)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &got); err != nil {
			t.Errorf("handler body %q: %v", b, err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, body := range []string{
		`{"project_id":"b3c4df83-6e55-43a9-8659-6ee837e576fc"}`,
		`{"project_id":"b3c4df83-6e55-43a9-8659-6ee837e576fc","dev":{"idle_timeout_minutes":60}}`,
	} {
		got = nil
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/workspaces", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s: status %d %s", body, rec.Code, rec.Body)
		}
		// Given a request that names no ports, assert that it reaches the service without any,
		// so the workspace takes its project's (workspaces.Service plan), not an empty list.
		var dev struct {
			Ports *json.RawMessage `json:"ports"`
		}
		if raw, ok := got["dev"]; ok {
			if err := json.Unmarshal(raw, &dev); err != nil {
				t.Fatal(err)
			}
		}
		if dev.Ports != nil {
			t.Errorf("%s reached the service with dev.ports %s", body, *dev.Ports)
		}
	}
}
