// Package httpx holds the HTTP plumbing shared by every listener: request ids, panic recovery,
// Host and Origin guards, CORS, and the contract's {detail} error shape.
package httpx

import (
	"encoding/json"
	"net/http"
)

// WriteError writes the contract's {detail} error body with the given status. Middleware uses it;
// handlers return an apierr.Error instead.
func WriteError(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The encode error is a broken client connection; nothing to report.
	_ = json.NewEncoder(w).Encode(map[string]string{"detail": detail})
}
