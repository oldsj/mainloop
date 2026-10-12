package merge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/kagent"
)

// Runtime is the trusted kagent read that resolves a merge tool's authority: the Agent a Session
// runs on, and the named template that Agent was created from. The sessions never supply either.
type Runtime interface {
	SessionAgent(ctx context.Context, sessionID string) (kagent.AgentRef, error)
	AgentTemplate(ctx context.Context, ref kagent.AgentRef) (string, error)
}

// Pin is what a consent was recorded for and the trusted mapping that gave its tool authority. It is
// stored with the candidate when evaluation begins and checked again in the final decision.
type Pin struct {
	// ResponseID is the hitl_responses row that carries the decision.
	ResponseID string `json:"response_id"`
	// KagentSessionID is the runtime the request was recorded for.
	KagentSessionID string `json:"kagent_session_id"`
	AgentNamespace  string `json:"agent_namespace"`
	AgentName       string `json:"agent_name"`
	// Provider, TemplateName and ConfigDigest are the template mapping evidence: the one operator
	// configuration that matched the Session's Agent template.
	Provider     string `json:"provider"`
	TemplateName string `json:"template_name"`
	ConfigDigest string `json:"config_digest"`
}

func (p Pin) agent() kagent.AgentRef {
	return kagent.AgentRef{Namespace: p.AgentNamespace, Name: p.AgentName}
}

// Consent is the owner's recorded approval of one exact merge call.
type Consent struct {
	// ResponseID is the hitl_responses row that carries the decision.
	ResponseID string
	// InvocationRequestID is the request_id the approved tool call carries.
	InvocationRequestID string
	Pin                 Pin
}

// errUntrusted means the Session's Agent does not map to exactly one operator configuration: an
// inline template, an unknown or duplicate template/provider pair, or a Session that is not the one
// recorded. Nothing is wrong with the read; the answer is no authority.
var errUntrusted = errors.New("merge: no trusted template mapping")

// mappingFor resolves the template mapping of a runtime from kagent: the Session must run on the
// recorded Agent, and that Agent's named template and the provider must select exactly one
// configuration. Any other result is errUntrusted; a failed read is returned as it is.
func (s *Service) mappingFor(ctx context.Context, kagentSessionID string, ref kagent.AgentRef, provider string) (config.MergeConfiguration, Pin, error) {
	if s.Runtime == nil {
		return config.MergeConfiguration{}, Pin{}, errUntrusted
	}
	live, err := s.Runtime.SessionAgent(ctx, kagentSessionID)
	if err != nil {
		return config.MergeConfiguration{}, Pin{}, fmt.Errorf("read session: %w", err)
	}
	if live != ref {
		return config.MergeConfiguration{}, Pin{}, errUntrusted
	}
	template, err := s.Runtime.AgentTemplate(ctx, ref)
	switch {
	case errors.Is(err, kagent.ErrNoTemplate):
		return config.MergeConfiguration{}, Pin{}, errUntrusted
	case err != nil:
		return config.MergeConfiguration{}, Pin{}, fmt.Errorf("read agent: %w", err)
	}
	var matches []config.MergeConfiguration
	for _, c := range s.Configs {
		if c.TemplateName == template && c.Provider == provider {
			matches = append(matches, c)
		}
	}
	if len(matches) != 1 {
		return config.MergeConfiguration{}, Pin{}, errUntrusted
	}
	return matches[0], Pin{
		KagentSessionID: kagentSessionID, AgentNamespace: ref.Namespace, AgentName: ref.Name,
		Provider: provider, TemplateName: template, ConfigDigest: digest(matches[0]),
	}, nil
}

// maxPayloadBytes mirrors the cap the HITL package puts on the canonical form of a payload.
const maxPayloadBytes = 128 * 1024

// callHash is the hash the HITL receipt records for a call's arguments: sha256 of the canonical JSON
// (sorted keys, no HTML escaping) of {proposal_id, request_id}.
func callHash(proposalID, requestID string) (string, bool) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]any{"proposal_id": proposalID, "request_id": requestID}); err != nil {
		return "", false
	}
	out := bytes.TrimRight(b.Bytes(), "\n")
	if len(out) > maxPayloadBytes {
		return "", false
	}
	sum := sha256.Sum256(out)
	return hex.EncodeToString(sum[:]), true
}

// findConsent looks through a session's live tool-approval decisions for the owner's approval of
// merging proposalID. A decision counts when, for one call of it:
//   - the request is the session's own, not a nested agent's (nested asks are never answerable, so
//     they cannot carry consent);
//   - it was recorded for the runtime the session is bound to now, which is live: the session is not
//     archived and its runtime not deleted;
//   - the tool is the protected merge tool under the name the trusted template mapping of the
//     session's Agent gives for the session's provider;
//   - the owner approved the call;
//   - the call's arguments were exactly this proposal_id and a request_id, and the receipt's argument
//     hash covers them.
//
// wantRequestID narrows the match to one invocation; empty accepts any and reports which. resolve
// returns the mapping for a decision's runtime; its error ends the search when it is not errUntrusted.
func findConsent(
	rows []dbq.ListLiveResponsesForSessionRow, ownerID, proposalID, wantRequestID string,
	resolve func(row dbq.ListLiveResponsesForSessionRow) (config.MergeConfiguration, Pin, error),
) (Consent, bool, error) {
	for _, row := range rows {
		if !live(row, ownerID) {
			continue
		}
		var req api.ToolApprovalRequest
		if json.Unmarshal(row.Payload, &req) != nil || req.Nested != nil {
			continue
		}
		var receipt api.DecisionReceipt
		if json.Unmarshal(row.Receipt, &receipt) != nil {
			continue
		}
		for _, call := range receipt.Calls {
			if call.Approved == nil || !*call.Approved || call.ToolName == nil || call.CallId == nil {
				continue
			}
			for _, tool := range req.Tools {
				if tool.CallId != *call.CallId || tool.Name != *call.ToolName {
					continue
				}
				pid, _ := tool.Args["proposal_id"].(string)
				rid, _ := tool.Args["request_id"].(string)
				if pid != proposalID || rid == "" || (wantRequestID != "" && rid != wantRequestID) {
					continue
				}
				if want, ok := callHash(pid, rid); !ok || want != call.ArgumentsHash || len(tool.Args) != 2 {
					continue
				}
				cfg, pin, err := resolve(row)
				switch {
				case errors.Is(err, errUntrusted):
					continue
				case err != nil:
					return Consent{}, false, err
				case cfg.PublicName() != *call.ToolName:
					continue
				}
				pin.ResponseID = row.ResponseID
				return Consent{ResponseID: row.ResponseID, InvocationRequestID: rid, Pin: pin}, true, nil
			}
		}
	}
	return Consent{}, false, nil
}

// live reports whether a decision was recorded for the session's current, live runtime.
func live(row dbq.ListLiveResponsesForSessionRow, ownerID string) bool {
	return row.SessionOwner == ownerID && row.ArchivedAt == nil && row.KagentDeletedAt == nil &&
		row.BoundKagentSessionID != nil && *row.BoundKagentSessionID == row.RequestKagentSessionID
}

// consentFor reads the proposal's session's live decisions and finds the approval.
func (s *Service) consentFor(ctx context.Context, ownerID, sessionID, proposalID, requestID string) (Consent, bool, error) {
	rows, err := dbq.New(s.Pool).ListLiveResponsesForSession(ctx, dbq.ListLiveResponsesForSessionParams{OwnerID: ownerID, SessionID: sessionID})
	if err != nil {
		return Consent{}, false, fmt.Errorf("read decisions: %w", err)
	}
	type key struct{ session, ns, name, kind string }
	type got struct {
		cfg config.MergeConfiguration
		pin Pin
		err error
	}
	cache := map[key]got{}
	return findConsent(rows, ownerID, proposalID, requestID, func(row dbq.ListLiveResponsesForSessionRow) (config.MergeConfiguration, Pin, error) {
		k := key{row.RequestKagentSessionID, row.AgentNamespace, row.AgentName, row.AgentKind}
		g, ok := cache[k]
		if !ok {
			g.cfg, g.pin, g.err = s.mappingFor(ctx, k.session, kagent.AgentRef{Namespace: k.ns, Name: k.name}, k.kind)
			cache[k] = g
		}
		return g.cfg, g.pin, g.err
	})
}
