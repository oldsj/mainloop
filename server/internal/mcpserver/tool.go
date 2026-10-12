package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/oldsj/mainloop/server/internal/agentauth"
)

// Tool is one MCP tool: its contract and its handler. Which actors see it is the job of
// [agentauth.Policy], not of the tool.
type Tool struct {
	Name        string
	Description string
	// InputSchema is the JSON Schema of the arguments, listed by tools/list. Every schema forbids
	// additional properties.
	InputSchema map[string]any
	Handle      func(ctx context.Context, actor agentauth.Actor, args json.RawMessage) (Result, error)
}

// Result is what a tool returns: text for the model and the same facts as structured content.
type Result struct {
	Text       string
	Structured map[string]any
}

// toolError is a failure the caller sees as an error result, in the Python server's
// "[code] message" form.
type toolError struct{ msg string }

func (e *toolError) Error() string { return e.msg }

func failure(code, message string) error {
	return &toolError{msg: fmt.Sprintf("[%s] %s", code, message)}
}

var errValidation = failure("validation", "invalid tool arguments")

// asToolError reports whether err is a failure to show the caller.
func asToolError(err error) (string, bool) {
	var te *toolError
	if errors.As(err, &te) {
		return te.msg, true
	}
	return "", false
}

// decode parses the arguments strictly: unknown fields are refused, and absent or null arguments
// mean an empty object.
func decode(raw json.RawMessage, into any) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return errValidation
	}
	if dec.More() {
		return errValidation
	}
	return nil
}

// text trims s and checks its length in characters, the way the Python models did.
func text(s string, minLen, maxLen int) (string, error) {
	s = strings.TrimSpace(s)
	if n := utf8.RuneCountInString(s); n < minLen || n > maxLen {
		return "", errValidation
	}
	return s, nil
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{
		"type": "object", "properties": properties, "required": required, "additionalProperties": false,
	}
}

func stringSchema(minLen, maxLen int) map[string]any {
	s := map[string]any{"type": "string"}
	if minLen > 0 {
		s["minLength"] = minLen
	}
	if maxLen > 0 {
		s["maxLength"] = maxLen
	}
	return s
}

func optional(schema map[string]any) map[string]any {
	return map[string]any{"anyOf": []any{schema, map[string]any{"type": "null"}}, "default": nil}
}
