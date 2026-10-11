// Package health collects the orchestration-health observations a native agent turn leaves in its
// kagent task, rolls them up per turn, and serves them to the owner.
//
// The collector is durable. The observations are stored as rows in the same transaction as the
// receipt that says they were read, and a recovery sweep rebuilds anything that was not stored
// from the kagent task itself, so a restart loses nothing. The routes only read.
package health

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Schema is the version of the observation format this package reads.
const Schema = "kagent.dev/orchestration-health/v1"

const schemaPrefix = "kagent.dev/orchestration-health/"

const maxCount = 1<<53 - 1

var (
	keyPattern     = regexp.MustCompile(`^[a-zA-Z0-9_.:-]+$`)
	versionPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]*$`)
)

// Event is one validated observation. Exactly one of Tool, Turn and Usage is set, by Kind.
type Event struct {
	Kind             string
	EventID          string
	ProducerEpoch    string
	Sequence         int64
	OccurredAt       time.Time
	RuntimeVersion   string
	TurnKey          string
	RuntimeSessionID string
	A2ATaskID        string

	Tool  *Tool
	Turn  *Turn
	Usage *Usage

	// payload is the typed event as stored: the source's own timestamp text (it has nanoseconds,
	// which time.Time keeps but a database column does not) and nothing the schema does not name.
	payload map[string]any
}

// Tool is one phase of a tool call: its start or its result.
type Tool struct {
	CallKey      string
	Phase        string
	ToolClass    string
	OperationKey string
	Action       string
	Path         *string
	Outcome      string
	Category     string
}

// Turn is a turn boundary.
type Turn struct {
	State    string
	Coverage string
}

// Usage is the token total of a turn. A later revision replaces an earlier one.
type Usage struct {
	SampleKey    string
	Revision     int64
	InputTokens  *int64
	OutputTokens *int64
	Coverage     string
}

// IsHealthPart reports whether an artifact part carries an observation: by the artifact name, or
// by a schema_version of this family in the data (a future version is then a recorded gap, not
// silently ignored).
func IsHealthPart(artifactName string, data any) bool {
	if strings.HasPrefix(artifactName, schemaPrefix) {
		return true
	}
	m, ok := data.(map[string]any)
	if !ok {
		return false
	}
	v, ok := m["schema_version"].(string)
	return ok && strings.HasPrefix(v, schemaPrefix)
}

// Payload returns the typed event as stored.
func (e Event) Payload() ([]byte, error) { return json.Marshal(e.payload) }

// ErrInvalid is the error of an observation that does not match the schema.
var ErrInvalid = errors.New("invalid health event")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

var commonKeys = []string{
	"a2a_task_id", "event_id", "kind", "occurred_at", "producer_epoch", "provider", "runtime_session_id",
	"runtime_version", "schema_version", "sequence", "source", "time_precision", "time_source", "turn_key",
}

// ParseEvent validates one observation as decoded from JSON. The schema is closed: the exact keys,
// no unknown ones, and every value in its enumeration, so an event that is not exactly v1 is
// refused instead of half-read.
func ParseEvent(raw any) (Event, error) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return Event{}, invalidf("not an object")
	}
	r := reader{obj: obj}
	kind := r.enum("kind", "turn", "tool", "usage")
	if r.err != nil {
		return Event{}, r.err
	}
	r.exactKeys(append([]string{kind}, commonKeys...)...)
	e := Event{Kind: kind}
	r.literal("schema_version", Schema)
	e.EventID = r.key("event_id")
	e.ProducerEpoch = r.key("producer_epoch")
	e.Sequence = r.integer("sequence", 1, 1_000_000)
	e.OccurredAt = r.timestamp("occurred_at")
	r.literal("time_source", "adapter_observed")
	r.literal("time_precision", "nanosecond")
	r.literal("provider", "claude")
	e.RuntimeVersion = r.version("runtime_version")
	r.literal("source", "claude_result_stream")
	e.TurnKey = r.key("turn_key")
	e.RuntimeSessionID = r.key("runtime_session_id")
	e.A2ATaskID = r.key("a2a_task_id")
	if r.err != nil {
		return Event{}, r.err
	}
	if e.TurnKey != e.ProducerEpoch || e.EventID != e.ProducerEpoch+":"+strconv.FormatInt(e.Sequence, 10) {
		return Event{}, invalidf("invalid producer identity")
	}
	switch kind {
	case "turn":
		e.Turn = parseTurn(&r)
	case "tool":
		e.Tool = parseTool(&r)
	case "usage":
		e.Usage = parseUsage(&r)
		if r.err == nil && e.Usage.SampleKey != e.TurnKey {
			r.err = invalidf("sample must belong to its turn")
		}
	}
	if r.err != nil {
		return Event{}, r.err
	}
	e.payload = r.out
	return e, nil
}

func parseTurn(r *reader) *Turn {
	sub := r.object("turn", "state", "coverage")
	if sub == nil {
		return nil
	}
	t := &Turn{State: sub.enum("state", "started", "finished", "failed"), Coverage: sub.enum("coverage", "complete", "partial")}
	r.adopt("turn", sub)
	return t
}

func parseTool(r *reader) *Tool {
	sub := r.object("tool", "call_key", "phase", "tool_class", "operation_key", "action", "path", "outcome", "category")
	if sub == nil {
		return nil
	}
	t := &Tool{
		CallKey:      sub.key("call_key"),
		Phase:        sub.enum("phase", "started", "finished"),
		ToolClass:    sub.enum("tool_class", "builtin", "shell", "mcp", "other"),
		OperationKey: sub.key("operation_key"),
		Action:       sub.enum("action", "read", "edit", "search", "other"),
		Path:         sub.path("path"),
		Outcome:      sub.enum("outcome", "success", "error", "denied", "unknown"),
		Category:     sub.enum("category", "none", "tool_error", "mcp_error", "permission_denied"),
	}
	r.adopt("tool", sub)
	return t
}

func parseUsage(r *reader) *Usage {
	sub := r.object("usage", "scope", "basis", "inclusion", "sample_key", "revision", "input_tokens", "output_tokens", "coverage")
	if sub == nil {
		return nil
	}
	sub.literal("scope", "turn")
	sub.literal("basis", "turn_total")
	sub.literal("inclusion", "tree_inclusive")
	u := &Usage{
		SampleKey:    sub.key("sample_key"),
		Revision:     sub.integer("revision", 1, 1<<62),
		InputTokens:  sub.count("input_tokens"),
		OutputTokens: sub.count("output_tokens"),
		Coverage:     sub.enum("coverage", "complete", "partial"),
	}
	r.adopt("usage", sub)
	return u
}

// reader reads typed fields out of a decoded JSON object and remembers the first problem. It
// copies each accepted value into out, so the stored payload is exactly what was validated.
type reader struct {
	obj map[string]any
	out map[string]any
	err error
}

func (r *reader) fail(format string, args ...any) {
	if r.err == nil {
		r.err = invalidf(format, args...)
	}
}

func (r *reader) put(k string, v any) {
	if r.out == nil {
		r.out = map[string]any{}
	}
	r.out[k] = v
}

func (r *reader) exactKeys(keys ...string) {
	if r.err != nil {
		return
	}
	if len(r.obj) != len(keys) {
		r.fail("unexpected or missing fields")
		return
	}
	for _, k := range keys {
		if _, ok := r.obj[k]; !ok {
			r.fail("missing field %s", k)
			return
		}
	}
}

func (r *reader) object(name string, keys ...string) *reader {
	if r.err != nil {
		return nil
	}
	m, ok := r.obj[name].(map[string]any)
	if !ok {
		r.fail("%s is not an object", name)
		return nil
	}
	sub := &reader{obj: m}
	sub.exactKeys(keys...)
	if sub.err != nil {
		r.err = sub.err
		return nil
	}
	return sub
}

func (r *reader) adopt(name string, sub *reader) {
	if sub.err != nil {
		r.fail("%s", strings.TrimPrefix(sub.err.Error(), ErrInvalid.Error()+": "))
		return
	}
	r.put(name, sub.out)
}

func (r *reader) str(k string) (string, bool) {
	if r.err != nil {
		return "", false
	}
	s, ok := r.obj[k].(string)
	if !ok {
		r.fail("%s is not a string", k)
	}
	return s, ok
}

func (r *reader) enum(k string, allowed ...string) string {
	s, ok := r.str(k)
	if !ok {
		return ""
	}
	for _, a := range allowed {
		if s == a {
			r.put(k, s)
			return s
		}
	}
	r.fail("%s is not an allowed value", k)
	return ""
}

func (r *reader) literal(k, want string) {
	if s, ok := r.str(k); ok {
		if s != want {
			r.fail("%s must be %s", k, want)
			return
		}
		r.put(k, s)
	}
}

func (r *reader) key(k string) string {
	s, ok := r.str(k)
	if !ok {
		return ""
	}
	if len(s) == 0 || len(s) > 128 || !keyPattern.MatchString(s) {
		r.fail("%s is not a valid key", k)
		return ""
	}
	r.put(k, s)
	return s
}

func (r *reader) version(k string) string {
	s, ok := r.str(k)
	if !ok {
		return ""
	}
	if len(s) > 32 || !versionPattern.MatchString(s) {
		r.fail("%s is not a valid version", k)
		return ""
	}
	r.put(k, s)
	return s
}

// number reads a JSON number that is a whole value. Numbers arrive from encoding/json as float64
// (the a2a data part is an any), so a fraction or a value beyond 2^53 is not a whole count.
func (r *reader) number(k string) (int64, bool) {
	if r.err != nil {
		return 0, false
	}
	switch v := r.obj[k].(type) {
	case float64:
		if v > maxCount || v < -maxCount || v != float64(int64(v)) {
			r.fail("%s is not a whole number", k)
			return 0, false
		}
		return int64(v), true
	case json.Number:
		n, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			r.fail("%s is not a whole number", k)
			return 0, false
		}
		return n, true
	case int:
		return int64(v), true
	case int64:
		return v, true
	}
	r.fail("%s is not a number", k)
	return 0, false
}

func (r *reader) integer(k string, lo, hi int64) int64 {
	n, ok := r.number(k)
	if !ok {
		return 0
	}
	if n < lo || n > hi {
		r.fail("%s is out of range", k)
		return 0
	}
	r.put(k, n)
	return n
}

// count is a token count: null or a whole number in 0..2^53-1.
func (r *reader) count(k string) *int64 {
	if r.err != nil {
		return nil
	}
	if r.obj[k] == nil {
		r.put(k, nil)
		return nil
	}
	n := r.integer(k, 0, maxCount)
	if r.err != nil {
		return nil
	}
	return &n
}

func (r *reader) timestamp(k string) time.Time {
	s, ok := r.str(k)
	if !ok {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		r.fail("%s is not an RFC 3339 timestamp with a timezone", k)
		return time.Time{}
	}
	r.put(k, s) // the source's own text keeps its nanoseconds
	return t
}

// path is null or a safe relative path of at most 256 characters.
func (r *reader) path(k string) *string {
	if r.err != nil {
		return nil
	}
	if r.obj[k] == nil {
		r.put(k, nil)
		return nil
	}
	s, ok := r.str(k)
	if !ok {
		return nil
	}
	if len(s) > 256 || !SafePath(s) {
		r.fail("%s is not a safe path", k)
		return nil
	}
	r.put(k, s)
	return &s
}

// SafePath reports whether a path may be stored: relative, normalised, inside the workspace, and
// not naming anything that holds credentials.
func SafePath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\n\r\x00?@\\") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		switch part {
		case "", ".", "..":
			return false
		}
		lower := strings.ToLower(part)
		if strings.HasPrefix(lower, ".env") || strings.Contains(lower, "credential") || strings.Contains(lower, "secret") ||
			lower == ".ssh" || lower == ".aws" || lower == ".kube" ||
			strings.HasSuffix(lower, ".pem") || strings.HasSuffix(lower, ".key") {
			return false
		}
	}
	return true
}

// Canonical is the JSON of an observation with sorted keys, for comparing two stored payloads.
func Canonical(raw []byte) ([]byte, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
