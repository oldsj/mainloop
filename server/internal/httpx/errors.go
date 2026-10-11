// Package httpx holds the HTTP plumbing shared by every listener: request ids, panic recovery,
// Host and Origin guards, CORS, and the contract's {detail} error shape.
package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Error is an error that maps to an HTTP status and the contract's HTTPError body.
type Error struct {
	Status int
	Detail string
}

func (e *Error) Error() string { return e.Detail }

// ErrNotImplemented is returned by operations no slice has implemented yet.
var ErrNotImplemented = &Error{Status: http.StatusNotImplemented, Detail: "Not implemented"}

// NewError builds an Error.
func NewError(status int, detail string) *Error { return &Error{Status: status, Detail: detail} }

// WriteError writes the {detail} body with the given status.
func WriteError(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The encode error is a broken client connection; nothing to report.
	_ = json.NewEncoder(w).Encode(map[string]string{"detail": detail})
}

// WriteErr maps err to a response. An *Error keeps its status and detail; anything else is a
// 500 whose cause stays in the logs, not in the body.
func WriteErr(w http.ResponseWriter, err error) {
	var he *Error
	if errors.As(err, &he) {
		WriteError(w, he.Status, he.Detail)
		return
	}
	WriteError(w, http.StatusInternalServerError, "Internal server error")
}
