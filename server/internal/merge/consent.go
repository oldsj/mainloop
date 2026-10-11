package merge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// Consent is the owner's recorded approval of one exact merge call.
type Consent struct {
	// ResponseID is the hitl_responses row that carries the decision.
	ResponseID string
	// InvocationRequestID is the request_id the approved tool call carries.
	InvocationRequestID string
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
//   - the tool is the protected merge tool under the name one operator configuration gives for the
//     session's provider;
//   - the owner approved the call;
//   - the call's arguments were exactly this proposal_id and a request_id, and the receipt's argument
//     hash covers them.
//
// wantRequestID narrows the match to one invocation; empty accepts any and reports which.
func findConsent(rows []dbq.ListLiveResponsesForSessionRow, configs []config.MergeConfiguration, proposalID, wantRequestID string) (Consent, bool) {
	for _, row := range rows {
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
			if !protectedName(configs, row.AgentKind, *call.ToolName) {
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
				if want, ok := callHash(pid, rid); ok && want == call.ArgumentsHash && len(tool.Args) == 2 {
					return Consent{ResponseID: row.ResponseID, InvocationRequestID: rid}, true
				}
			}
		}
	}
	return Consent{}, false
}

// protectedName reports whether name is the merge tool's public name for the provider under one of
// the operator's configurations.
func protectedName(configs []config.MergeConfiguration, provider, name string) bool {
	for _, c := range configs {
		if c.Provider == provider && c.PublicName() == name {
			return true
		}
	}
	return false
}

// consentFor reads the proposal's session's live decisions and finds the approval.
func (s *Service) consentFor(ctx context.Context, ownerID, sessionID, proposalID, requestID string) (Consent, bool, error) {
	rows, err := dbq.New(s.Pool).ListLiveResponsesForSession(ctx, dbq.ListLiveResponsesForSessionParams{OwnerID: ownerID, SessionID: sessionID})
	if err != nil {
		return Consent{}, false, fmt.Errorf("read decisions: %w", err)
	}
	c, ok := findConsent(rows, s.Configs, proposalID, requestID)
	return c, ok, nil
}
