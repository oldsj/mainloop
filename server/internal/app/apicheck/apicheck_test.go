package apicheck

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recordingTB stands in for the test being checked, so a violation can be observed rather than
// failing this test.
type recordingTB struct {
	testing.TB
	errors []string
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func serve(t *testing.T, path string, status int, body string) []string {
	t.Helper()
	rec := &recordingTB{TB: t}
	h := Wrap(rec, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	return rec.errors
}

func TestDeclaredResponsePasses(t *testing.T) {
	if errs := serve(t, "/health", 200, `{"status":"healthy","database":"ok"}`); len(errs) != 0 {
		t.Fatalf("a declared response failed: %v", errs)
	}
	if errs := serve(t, "/health", 503, `{"detail":"Database unavailable"}`); len(errs) != 0 {
		t.Fatalf("a declared 503 failed: %v", errs)
	}
}

func TestUndeclaredStatusFails(t *testing.T) {
	errs := serve(t, "/health", 418, `{"detail":"teapot"}`)
	if len(errs) != 1 || !strings.Contains(errs[0], "418") {
		t.Fatalf("an undeclared status was not caught: %v", errs)
	}
}

func TestWrongShapeFails(t *testing.T) {
	if errs := serve(t, "/health", 200, `{"status":"healthy"}`); len(errs) != 1 {
		t.Fatalf("a body missing a required field was not caught: %v", errs)
	}
	if errs := serve(t, "/health", 503, `{"message":"x"}`); len(errs) != 1 {
		t.Fatalf("an error body of the wrong shape was not caught: %v", errs)
	}
}

// /activity declares maxItems: 100, so a list past it is a contract break the tests must see.
func TestListPastMaxItemsFails(t *testing.T) {
	item := `{"id":"a","title":"x","content":"c","href":null,"created_at":"2026-01-01T00:00:00Z","queue_item_id":null,"dismissible":false}`
	few := "[" + item + "]"
	if errs := serve(t, "/activity", 200, few); len(errs) != 0 {
		t.Fatalf("a valid list failed: %v", errs)
	}
	many := "[" + strings.Repeat(item+",", 100) + item + "]"
	errs := serve(t, "/activity", 200, many)
	if len(errs) != 1 || !strings.Contains(errs[0], "number of items") {
		t.Fatalf("101 items did not fail on maxItems: %v", errs)
	}
}
