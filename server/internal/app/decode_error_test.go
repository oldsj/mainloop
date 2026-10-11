package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestDecodeError(t *testing.T) {
	handler := requestDecodeError(slog.New(slog.NewTextHandler(io.Discard, nil)))
	cases := []struct {
		name   string
		err    error
		status int
		detail string
	}{
		{"body over the cap, wrapped by the decoder", fmt.Errorf("decode: %w", &http.MaxBytesError{Limit: 1}), http.StatusRequestEntityTooLarge, "Request body too large"},
		{"malformed body", errors.New("invalid character 'x'"), http.StatusBadRequest, "Invalid request body"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler(rec, httptest.NewRequest(http.MethodPost, "/chat", nil), c.err)
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not {detail}: %q", rec.Body.String())
			}
			if rec.Code != c.status || body["detail"] != c.detail {
				t.Fatalf("got %d %q, want %d %q", rec.Code, body["detail"], c.status, c.detail)
			}
		})
	}
}
