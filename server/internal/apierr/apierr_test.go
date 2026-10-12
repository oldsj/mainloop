package apierr_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/apierr"
)

func TestWrite(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		body   string
	}{
		{"detail", apierr.Conflict("Session is archived"), 409, `{"detail":"Session is archived"}`},
		{"wrapped", fmt.Errorf("outer: %w", apierr.NotFound("Session not found")), 404, `{"detail":"Session not found"}`},
		{"unavailable", apierr.Unavailable("Tasks unavailable"), 503, `{"detail":"Tasks unavailable"}`},
		{"validation", apierr.Invalid("the body is required"), 422, `{"detail":[{"loc":[],"msg":"the body is required","type":"value_error"}]}`},
		{"reason", apierr.Reason(http.StatusForbidden, "not_yours"), 403, `{"detail":{"reason":"not_yours"}}`},
		{"not implemented", apierr.NotImplemented, 501, `{"detail":"Not implemented"}`},
		{"unknown", errors.New("pq: password authentication failed for hunter2"), 500, `{"detail":"Internal server error"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			apierr.Write(rec, tt.err)
			if rec.Code != tt.status || strings.TrimSpace(rec.Body.String()) != tt.body {
				t.Errorf("got %d %q", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("content type = %q", ct)
			}
		})
	}
}
