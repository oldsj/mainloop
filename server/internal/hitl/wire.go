package hitl

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
)

// Bounds from the kagent HITL wire contract.
const (
	maxPayloadBytes = 128 << 10
	maxTools        = 100
	maxQuestions    = 100
	maxIdentifier   = 2048
	maxHint         = 4096
	maxQuestionText = 8192
	maxChoice       = 4096
	maxAnswerText   = 8192
	maxReason       = 8192
)

// canonical returns v as compact JSON with sorted object keys, the form that is hashed. Numbers
// keep their written form.
func canonical(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	out := bytes.TrimRight(buf.Bytes(), "\n")
	if len(out) > maxPayloadBytes {
		return nil, errors.New("the HITL payload exceeds 128 KiB")
	}
	return out, nil
}

// canonicalRaw canonicalizes JSON text.
func canonicalRaw(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return canonical(v)
}

func hashOf(canon []byte) string {
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// requestID names an ask by where it was posted and what it says. The same ask observed twice has
// one id; a changed ask, or the same words in a new status message, has another.
func requestID(kagentSessionID, taskID, statusMessageID, requestHash string) string {
	h := sha256.New()
	for _, p := range []string{kagentSessionID, taskID, statusMessageID, requestHash} {
		_, _ = fmt.Fprintf(h, "%d:%s|", len(p), p)
	}
	return "hitl_" + hex.EncodeToString(h.Sum(nil))[:32]
}

// observed is a validated ask read from a task's status message.
type observed struct {
	kind            dbstate.HITLKind
	payload         []byte // canonical
	hash            string
	statusMessageID string
	tools           *api.ToolApprovalRequest
	ask             *api.AskUserRequest
}

var errNotHITL = errors.New("the status message carries no HITL request")

// readRequest decodes and validates the ask in a task's status message. errNotHITL means the
// message is not an ask at all; any other error means it is one that cannot be shown.
func readRequest(msg *a2a.Message) (*observed, error) {
	if msg == nil || !slices.Contains(msg.Extensions, kagent.HITLExtension) {
		return nil, errNotHITL
	}
	raw, ok := msg.Metadata[kagent.HITLExtension]
	if !ok {
		return nil, errNotHITL
	}
	if msg.ID == "" {
		return nil, errors.New("the status message has no id")
	}
	if len(msg.ID) > maxIdentifier {
		return nil, errors.New("the status message id is too long")
	}
	body, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("the HITL request is not JSON: %w", err)
	}
	canon, err := canonicalRaw(body)
	if err != nil {
		return nil, err
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(canon, &head); err != nil {
		return nil, errors.New("the HITL request is not an object")
	}
	o := &observed{payload: canon, hash: hashOf(canon), statusMessageID: msg.ID}
	switch head.Type {
	case "tool_approval_request":
		var r api.ToolApprovalRequest
		if err := json.Unmarshal(canon, &r); err != nil {
			return nil, fmt.Errorf("bad tool approval request: %w", err)
		}
		if err := checkToolRequest(&r); err != nil {
			return nil, err
		}
		o.kind, o.tools = dbstate.HITLToolApproval, &r
	case "ask_user_request":
		var r api.AskUserRequest
		if err := json.Unmarshal(canon, &r); err != nil {
			return nil, fmt.Errorf("bad ask-user request: %w", err)
		}
		if err := checkAskRequest(&r); err != nil {
			return nil, err
		}
		o.kind, o.ask = dbstate.HITLAskUser, &r
	default:
		return nil, errors.New("the HITL request has an unsupported type")
	}
	return o, nil
}

func checkIdent(what, v string) error {
	if v == "" || len(v) > maxIdentifier {
		return fmt.Errorf("%s must be 1 to %d characters", what, maxIdentifier)
	}
	return nil
}

func checkTools(tools []api.HITLTool) error {
	if len(tools) < 1 || len(tools) > maxTools {
		return fmt.Errorf("a request has 1 to %d tools", maxTools)
	}
	seen := map[string]bool{}
	for _, t := range tools {
		for what, v := range map[string]string{"tool id": t.Id, "call id": t.CallId, "tool name": t.Name} {
			if err := checkIdent(what, v); err != nil {
				return err
			}
		}
		if t.Args == nil {
			return errors.New("a tool needs args")
		}
		if seen[t.Id] {
			return errors.New("duplicate pending tool id")
		}
		seen[t.Id] = true
	}
	return nil
}

func checkNested(n *api.NestedHITLRequest) error {
	if n == nil {
		return nil
	}
	if err := checkIdent("nested task id", n.TaskId); err != nil {
		return err
	}
	if err := checkIdent("nested context id", n.ContextId); err != nil {
		return err
	}
	if len(n.SubagentName) > maxIdentifier {
		return errors.New("the subagent name is too long")
	}
	return checkTools(n.Tools)
}

func checkToolRequest(r *api.ToolApprovalRequest) error {
	if len(r.Hint) > maxHint {
		return errors.New("the hint is too long")
	}
	if err := checkTools(r.Tools); err != nil {
		return err
	}
	return checkNested(r.Nested)
}

func checkAskRequest(r *api.AskUserRequest) error {
	if err := checkIdent("request id", r.Id); err != nil {
		return err
	}
	if len(r.Questions) < 1 || len(r.Questions) > maxQuestions {
		return fmt.Errorf("a request has 1 to %d questions", maxQuestions)
	}
	for _, q := range r.Questions {
		if q.Question == "" || len(q.Question) > maxQuestionText {
			return errors.New("a question must be 1 to 8192 characters")
		}
		if q.Choices != nil {
			if len(*q.Choices) > maxQuestions {
				return errors.New("a question has too many choices")
			}
			for _, c := range *q.Choices {
				if len(c) > maxChoice {
					return errors.New("a choice is too long")
				}
			}
		}
	}
	return checkNested(r.Nested)
}

// decision is a validated owner response: what is sent to kagent, and what the receipt shows.
type decision struct {
	native   map[string]any // the wire form, without reviewed_context
	tools    *api.ToolApprovalResponse
	ask      *api.AskUserResponse
	approved map[string]bool // tool id -> approved, for the receipt calls
}

// decodeDecision reads the response union. An unknown type is a validation error.
func decodeDecision(in api.HITLDecisionInput_Response) (*decision, error) {
	raw, err := in.MarshalJSON()
	if err != nil {
		return nil, errors.New("the response is not valid JSON")
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, errors.New("the response is not an object")
	}
	d := &decision{}
	switch head.Type {
	case "tool_approval_response":
		var r api.ToolApprovalResponse
		if err := strictUnmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("bad tool approval response: %w", err)
		}
		d.tools = &r
	case "ask_user_response":
		var r api.AskUserResponse
		if err := strictUnmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("bad ask-user response: %w", err)
		}
		d.ask = &r
	default:
		return nil, errors.New("the response type must be tool_approval_response or ask_user_response")
	}
	return d, nil
}

func strictUnmarshal(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// validate checks the response against the ask it answers, and fills native.
func (d *decision) validate(o *observed) error {
	switch {
	case d.tools != nil:
		if o.tools == nil {
			return errors.New("this request is a question, not a tool approval")
		}
		return d.validateTools(o.tools)
	case d.ask != nil:
		if o.ask == nil {
			return errors.New("this request is a tool approval, not a question")
		}
		return d.validateAsk(o.ask)
	}
	return errors.New("the response is empty")
}

func (d *decision) validateTools(req *api.ToolApprovalRequest) error {
	r := d.tools
	if len(r.Approvals) < 1 || len(r.Approvals) > maxTools {
		return fmt.Errorf("a response has 1 to %d approvals", maxTools)
	}
	required := map[string]bool{}
	allowed := map[string]bool{}
	for _, t := range req.Tools {
		required[t.Id], allowed[t.Id] = true, true
	}
	if req.Nested != nil {
		for _, t := range req.Nested.Tools {
			allowed[t.Id] = true
		}
	}
	seen := map[string]bool{}
	d.approved = map[string]bool{}
	approvals := make([]map[string]any, 0, len(r.Approvals))
	for _, a := range r.Approvals {
		if err := checkIdent("approval id", a.Id); err != nil {
			return err
		}
		if !allowed[a.Id] {
			return fmt.Errorf("approval %q names no pending tool", a.Id)
		}
		if seen[a.Id] {
			return fmt.Errorf("tool %q is answered twice", a.Id)
		}
		seen[a.Id] = true
		if a.RejectionReason != nil {
			if a.Approved && *a.RejectionReason != "" {
				return errors.New("an approved tool cannot have a rejection reason")
			}
			if len(*a.RejectionReason) > maxReason {
				return errors.New("a rejection reason is too long")
			}
		}
		d.approved[a.Id] = a.Approved
		item := map[string]any{"id": a.Id, "approved": a.Approved}
		if a.RejectionReason != nil {
			item["rejection_reason"] = *a.RejectionReason
		}
		approvals = append(approvals, item)
	}
	for id := range required {
		if !seen[id] {
			return fmt.Errorf("tool %q has no answer", id)
		}
	}
	if r.ReviewedContext != nil {
		if len(*r.ReviewedContext) > maxTools {
			return errors.New("reviewed_context has too many entries")
		}
		for id, digest := range *r.ReviewedContext {
			if !allowed[id] {
				return fmt.Errorf("reviewed_context names no pending tool %q", id)
			}
			if !isDigest(digest) {
				return errors.New("a reviewed_context digest must be 64 lowercase hex characters")
			}
		}
	}
	d.native = map[string]any{"type": "tool_approval_response", "approvals": approvals}
	return nil
}

func isDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (d *decision) validateAsk(req *api.AskUserRequest) error {
	r := d.ask
	if r.Id != req.Id {
		return errors.New("the response id does not match the question")
	}
	if len(r.Answers) != len(req.Questions) {
		return fmt.Errorf("the request has %d questions and the response %d answers", len(req.Questions), len(r.Answers))
	}
	answers := make([]map[string]any, 0, len(r.Answers))
	for i, a := range r.Answers {
		q := req.Questions[i]
		if len(a.Answer) < 1 || len(a.Answer) > maxQuestions {
			return fmt.Errorf("question %d needs 1 to %d answers", i+1, maxQuestions)
		}
		if !q.Multiple && len(a.Answer) != 1 {
			return fmt.Errorf("question %d takes one answer", i+1)
		}
		for _, text := range a.Answer {
			if len(text) > maxAnswerText {
				return fmt.Errorf("an answer to question %d is too long", i+1)
			}
			if q.Choices != nil && len(*q.Choices) > 0 && !slices.Contains(*q.Choices, text) {
				return fmt.Errorf("an answer to question %d is not one of its choices", i+1)
			}
		}
		answers = append(answers, map[string]any{"answer": a.Answer})
	}
	d.native = map[string]any{"type": "ask_user_response", "id": r.Id, "answers": answers}
	return nil
}

// hash is the digest of the native response, which tells a repeat of an action from a different
// decision under the same action id. The reviewed_context is part of it: it is what the owner saw.
func (d *decision) hash() (string, error) {
	body := map[string]any{"native": d.native}
	if d.tools != nil && d.tools.ReviewedContext != nil {
		body["reviewed_context"] = *d.tools.ReviewedContext
	}
	canon, err := canonical(body)
	if err != nil {
		return "", err
	}
	return hashOf(canon), nil
}

// responseMessage builds the A2A message that carries a response: the native response sits in the
// HITL extension's metadata, and the text part is a label.
func responseMessage(native map[string]any) map[string]any {
	return map[string]any{kagent.HITLExtension: native}
}
