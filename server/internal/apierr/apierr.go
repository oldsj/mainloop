// Package apierr is the one way a handler answers with an error status.
//
// A handler returns an *Error as its error and the strict server's response error handler writes
// it with [Write]. Handlers do not return the generated per-status response types for error
// statuses: those types exist only for the statuses an operation declares, and a status chosen at
// runtime would otherwise need a switch per operation. The cost is that the compiler no longer
// checks that an operation declares the status it returns, so apicheck (in app's tests) validates
// every response a test produces against api/openapi.yaml, and an undeclared status or a body of
// the wrong shape fails there.
//
// Three body shapes exist, matching the contract: [New] and its helpers write HTTPError
// ({"detail": "..."}), [Invalid] writes HTTPValidationError (a 422 with a detail list), and
// [Reason] writes TaskError ({"detail": {"reason": "..."}}). An error that is not an *Error is a
// 500 whose cause stays in the logs, not in the body.
package apierr

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Error is an error that maps to an HTTP status and a JSON body.
type Error struct {
	Status int
	// Message is the text Error returns: the HTTPError detail, or the validation message.
	Message string
	body    any
}

func (e *Error) Error() string { return e.Message }

type httpError struct {
	Detail string `json:"detail"`
}

type codedError struct {
	Detail string `json:"detail"`
	Code   string `json:"code"`
}

type validationError struct {
	Loc  []string `json:"loc"`
	Msg  string   `json:"msg"`
	Type string   `json:"type"`
}

type validationBody struct {
	Detail []validationError `json:"detail"`
}

type reasonBody struct {
	Detail struct {
		Reason string `json:"reason"`
	} `json:"detail"`
}

// New builds an error with the contract's HTTPError body.
func New(status int, detail string) *Error {
	return &Error{Status: status, Message: detail, body: httpError{Detail: detail}}
}

// ConflictCode is a 409 with the contract's CodedHTTPError body: the human message and a stable
// machine code the client switches on. (The TaskError shape of [Reason] has no message, and the
// routes that use this one already show `detail` as text.)
func ConflictCode(detail, code string) *Error {
	return &Error{Status: http.StatusConflict, Message: detail, body: codedError{Detail: detail, Code: code}}
}

// Forbidden is a 403.
func Forbidden(detail string) *Error { return New(http.StatusForbidden, detail) }

// NotFound is a 404.
func NotFound(detail string) *Error { return New(http.StatusNotFound, detail) }

// Conflict is a 409.
func Conflict(detail string) *Error { return New(http.StatusConflict, detail) }

// BadGateway is a 502.
func BadGateway(detail string) *Error { return New(http.StatusBadGateway, detail) }

// Unavailable is a 503: the route is declared but the dependency behind it is not configured, or
// is starting or stopping.
func Unavailable(detail string) *Error { return New(http.StatusServiceUnavailable, detail) }

// Invalid is a 422 with the contract's HTTPValidationError body, for a request the contract's
// schema accepts but the handler refuses.
func Invalid(msg string) *Error {
	return &Error{
		Status: http.StatusUnprocessableEntity, Message: msg,
		body: validationBody{Detail: []validationError{{Loc: []string{}, Msg: msg, Type: "value_error"}}},
	}
}

// Reason builds an error with the contract's TaskError body: a stable machine code the client
// switches on.
func Reason(status int, reason string) *Error {
	var b reasonBody
	b.Detail.Reason = reason
	return &Error{Status: status, Message: reason, body: b}
}

// NotImplemented is returned by operations no slice has implemented yet.
var NotImplemented = New(http.StatusNotImplemented, "Not implemented")

// Write writes err as the response: an *Error keeps its status and body, anything else is a 500.
func Write(w http.ResponseWriter, err error) {
	var e *Error
	if !errors.As(err, &e) {
		e = New(http.StatusInternalServerError, "Internal server error")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	// The encode error is a broken client connection; nothing to report.
	_ = json.NewEncoder(w).Encode(e.body)
}

// As returns the *Error in err's chain.
func As(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// StatusOf is the HTTP status err answers with: its *Error's status, or 500 for anything else.
func StatusOf(err error) int {
	if e, ok := As(err); ok {
		return e.Status
	}
	return http.StatusInternalServerError
}
