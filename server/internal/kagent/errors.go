package kagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
)

// Every call returns one of the error types below, or the caller's own context error before
// anything left. They answer one question: did the request leave, and was it accepted?
//
//	Unreachable      never left (dial, DNS or TLS failure). Safe to send again.
//	SendNotAccepted  kagent refused the send and said retrying the same message is safe.
//	OutcomeUnknown   it may have been accepted and the answer was lost. Never send again;
//	                 observe with GetTask or ListTasks instead.
//	TaskNotFound     kagent has no such task.
//	SessionError     a SessionService or AgentService call failed with a definite status.
//	ConfigError      kagent refused our credentials, or we cannot read them.
//
// Anything else from the A2A endpoint is an *A2AError, or a *ProtocolError when the reply is not a
// JSON-RPC envelope at all. A ProtocolError says nothing about delivery: treat it as uncertain,
// the same as OutcomeUnknown, never as not sent.

const (
	a2aErrorDomain  = "a2a-protocol.org"
	reasonNotAccept = "KAGENT_SEND_NOT_ACCEPTED"
	reasonNoTask    = "TASK_NOT_FOUND"

	// codeUnsupported and notReadyMessage are kagent's refusal of a send to a Session that is not
	// READY (A2A ErrUnsupportedOperation).
	codeUnsupported = -32004
	notReadyMessage = "Session cannot accept work during a lifecycle operation"

	errorInfoType = "google.rpc.ErrorInfo"

	defaultRetryAfter = 100 * time.Millisecond
)

// Unreachable means the request never left: the connection was not established.
type Unreachable struct{ Err error }

func (e *Unreachable) Error() string { return "kagent gateway unreachable: " + e.Err.Error() }
func (e *Unreachable) Unwrap() error { return e.Err }

// OutcomeUnknown means the request may have been accepted but the result was not observed.
type OutcomeUnknown struct {
	Op     string
	Detail string
	Err    error
}

func (e *OutcomeUnknown) Error() string {
	msg := e.Op + " outcome unknown"
	if e.Detail != "" {
		msg += " (" + e.Detail + ")"
	}
	return msg
}

func (e *OutcomeUnknown) Unwrap() error { return e.Err }

// ConfigError means kagent refused the credentials (HTTP 401 or 403, gRPC status 7 or 16, or
// an A2A auth refusal) or the token file cannot be used. Never replace or resend.
//
// Credentials is true when the refusal is of our own bearer (HTTP 401, gRPC Unauthenticated, A2A
// UNAUTHENTICATED, or a token file we cannot use), so every call would fail the same way. A 403,
// PermissionDenied or A2A UNAUTHORIZED can be about one Agent or conversation alone.
type ConfigError struct {
	Msg         string
	Credentials bool
}

func (e *ConfigError) Error() string { return e.Msg }

// SessionError is a definite SessionService or AgentService failure.
type SessionError struct {
	Op     string
	Code   connect.Code // equals the gRPC status
	Detail string
	// Wire is true when kagent itself sent the status. connect also synthesizes codes on the client,
	// for example InvalidArgument for a reply it could not decode or that was cut short; those prove
	// nothing about what kagent did.
	Wire bool
}

func (e *SessionError) Error() string {
	msg := fmt.Sprintf("%s failed (grpc %d)", e.Op, int(e.Code))
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// A2AError is a JSON-RPC error from the A2A endpoint with its google.rpc.ErrorInfo.
type A2AError struct {
	Code     int
	Message  string
	Domain   string
	Reason   string
	Metadata map[string]string
}

func (e *A2AError) Error() string { return fmt.Sprintf("A2A error %d: %s", e.Code, e.Message) }

// RetryAfter is kagent's hint for how long to wait before sending the same message again.
func (e *A2AError) RetryAfter() time.Duration {
	ms, err := strconv.ParseFloat(e.Metadata["retryAfterMs"], 64)
	if err != nil || ms < 0 {
		return defaultRetryAfter
	}
	return time.Duration(ms * float64(time.Millisecond))
}

// SendNotAccepted means kagent did not accept the message. Sending the same message again
// is safe.
type SendNotAccepted struct{ A2AError }

// Unwrap lets errors.As find the embedded *A2AError.
func (e *SendNotAccepted) Unwrap() error { return &e.A2AError }

// TaskNotFound means kagent has no such task.
type TaskNotFound struct{ A2AError }

// Unwrap lets errors.As find the embedded *A2AError.
func (e *TaskNotFound) Unwrap() error { return &e.A2AError }

// ProtocolError is a reply that is neither a JSON-RPC envelope nor a gateway failure, such as a
// redirect or an unexpected 4xx. For a send it means the message may have been delivered, so
// delivery code must treat it as uncertain and observe with FindTaskForMessage.
type ProtocolError struct {
	Op     string
	Status int
}

func (e *ProtocolError) Error() string { return fmt.Sprintf("%s failed (HTTP %d)", e.Op, e.Status) }

// NotSent reports whether err proves the message was not delivered, so the caller may send
// the same message again: the gateway was unreachable or kagent refused it.
func NotSent(err error) bool {
	var u *Unreachable
	var n *SendNotAccepted
	return errors.As(err, &u) || errors.As(err, &n)
}

// notSentError tags a transport failure that happened before any connection existed.
type notSentError struct{ err error }

func (e *notSentError) Error() string { return e.err.Error() }
func (e *notSentError) Unwrap() error { return e.err }

// statusError tags an HTTP 5xx from the gateway on a call whose body is not parsed.
type statusError struct{ status int }

func (e *statusError) Error() string { return "HTTP " + strconv.Itoa(e.status) }

func authRefused(detail string, credentials bool) *ConfigError {
	return &ConfigError{Msg: "kagent control service configuration failure (" + detail + ")", Credentials: credentials}
}

// classifyTransport maps an error from the HTTP layer, with no reply, to a typed error.
func classifyTransport(op string, err error) error {
	var cfg *ConfigError
	if errors.As(err, &cfg) {
		return cfg
	}
	var ns *notSentError
	if errors.As(err, &ns) {
		return &Unreachable{Err: ns.err}
	}
	var st *statusError
	if errors.As(err, &st) {
		return &OutcomeUnknown{Op: op, Detail: "HTTP " + strconv.Itoa(st.status), Err: err}
	}
	return &OutcomeUnknown{Op: op, Detail: errorKind(err), Err: err}
}

// errorKind names the failure without echoing its text.
func errorKind(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return fmt.Sprintf("%T", err)
}

// classifyConnect maps the error from a unary connect call.
func classifyConnect(op string, err error) error {
	// A transport failure wrapped by connect still carries its tag.
	var cfg *ConfigError
	var ns *notSentError
	var st *statusError
	var ce *connect.Error
	if errors.As(err, &cfg) || errors.As(err, &ns) || errors.As(err, &st) || !errors.As(err, &ce) {
		return classifyTransport(op, err)
	}
	if errors.Is(err, errAmbiguousWire) {
		return &OutcomeUnknown{Op: op, Detail: "ambiguous reply", Err: err}
	}
	code := ce.Code()
	switch {
	case code == connect.CodePermissionDenied || code == connect.CodeUnauthenticated:
		return authRefused("grpc "+strconv.Itoa(int(code)), code == connect.CodeUnauthenticated)
	case code == connect.CodeDeadlineExceeded || code == connect.CodeAborted ||
		code == connect.CodeInternal || code == connect.CodeUnavailable:
		// The companion can answer Aborted after it reserved a Session, so it proves nothing.
		return &OutcomeUnknown{Op: op, Detail: "grpc " + strconv.Itoa(int(code)), Err: err}
	case (code == connect.CodeCanceled || code == connect.CodeUnknown) && !connect.IsWireError(err):
		// connect reports its own cancellation and transport failures this way.
		return &OutcomeUnknown{Op: op, Detail: errorKind(err), Err: err}
	}
	return &SessionError{Op: op, Code: code, Detail: ce.Message(), Wire: connect.IsWireError(err)}
}

// classifyRPCError turns a JSON-RPC error object into a typed error by its ErrorInfo. A JSON-RPC
// code alone proves nothing; the one exception is not-ready, which matches on the code plus its text.
func classifyRPCError(code int, message string, data json.RawMessage) error {
	e := A2AError{Code: code, Message: message, Metadata: map[string]string{}}
	for _, item := range errorDetails(data) {
		if !strings.HasSuffix(item.Type, errorInfoType) {
			continue
		}
		e.Domain, e.Reason = item.Domain, item.Reason
		for k, v := range item.Metadata {
			e.Metadata[k] = fmt.Sprint(v)
		}
		break
	}
	switch {
	case e.Reason == "UNAUTHENTICATED" || e.Reason == "PERMISSION_DENIED" || e.Reason == "UNAUTHORIZED":
		return authRefused("A2A auth refusal", e.Reason == "UNAUTHENTICATED")
	case code == codeUnsupported && message == notReadyMessage:
		// kagent answers a send to a Session that is suspended, resuming or otherwise changing before it
		// reserves a dispatch, so nothing ran and the same message can be sent again. The refusal has
		// no marker, so only this code and text count.
		return &SendNotAccepted{A2AError: e}
	case e.Domain == a2aErrorDomain && e.Metadata["reason"] == reasonNotAccept:
		return &SendNotAccepted{A2AError: e}
	case e.Reason == reasonNoTask:
		return &TaskNotFound{A2AError: e}
	}
	return &e
}

type errorDetail struct {
	Type     string         `json:"@type"`
	Domain   string         `json:"domain"`
	Reason   string         `json:"reason"`
	Metadata map[string]any `json:"metadata"`
}

// errorDetails accepts the details as a list, or as one object.
func errorDetails(data json.RawMessage) []errorDetail {
	if len(data) == 0 {
		return nil
	}
	var list []errorDetail
	if json.Unmarshal(data, &list) == nil {
		return list
	}
	var one errorDetail
	if json.Unmarshal(data, &one) == nil {
		return []errorDetail{one}
	}
	return nil
}

// emptyArg is the definite failure of a call that was never made because an identifier was
// empty; a blank id would otherwise ask kagent about nothing in particular.
func emptyArg(op, what string) *SessionError {
	return &SessionError{Op: op, Code: connect.CodeInvalidArgument, Detail: what + " is empty"}
}
