package httpx

import (
	"errors"
	"net/http"
)

// MaxBody caps request bodies at limit bytes. A body that grows past the cap makes reads fail
// with *http.MaxBytesError, which the layer that reads it maps to 413 (see IsBodyTooLarge).
func MaxBody(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				WriteError(w, http.StatusRequestEntityTooLarge, "Request body too large")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

// IsBodyTooLarge reports whether err comes from a body that went past the MaxBody cap.
func IsBodyTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}
