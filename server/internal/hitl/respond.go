package hitl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

const gateway = "kagent"

// maxActionID is the contract's limit on HITLDecisionInput.action_id.
const maxActionID = 2048

// reasonNested says why a delegated request is shown but cannot be answered. The leaf rule, which
// decides what an answer to a nested request means, is G10's.
const reasonNested = "This request comes from a delegated agent. Answering those requests is not supported yet."

// isNested reports whether the ask was raised by a delegated (nested) task.
func isNested(o *observed) bool {
	return (o.tools != nil && o.tools.Nested != nil) || (o.ask != nil && o.ask.Nested != nil)
}

// parseStored reads a stored request payload back into the validated form.
func parseStored(r dbq.HitlRequest) (*observed, error) {
	o := &observed{kind: r.Kind, payload: r.Payload, hash: r.RequestHash, statusMessageID: r.StatusMessageID}
	switch r.Kind {
	case dbstate.HITLToolApproval:
		var v api.ToolApprovalRequest
		if err := json.Unmarshal(r.Payload, &v); err != nil {
			return nil, err
		}
		o.tools = &v
	case dbstate.HITLAskUser:
		var v api.AskUserRequest
		if err := json.Unmarshal(r.Payload, &v); err != nil {
			return nil, err
		}
		o.ask = &v
	default:
		return nil, fmt.Errorf("unknown request kind %q", r.Kind)
	}
	return o, nil
}

func outerOf(r dbq.HitlRequest) api.ContinuationIdentity {
	return api.ContinuationIdentity{
		ContextId: r.ContextID, Endpoint: r.AgentNamespace + "/" + r.AgentName, Gateway: gateway,
		RequestHash: r.RequestHash, RuntimeSessionId: r.KagentSessionID, StatusMessageId: r.StatusMessageID,
		TaskId: r.TaskID,
	}
}

func leafOf(r dbq.HitlRequest) api.LeafIdentity {
	return api.LeafIdentity{
		ContextId: r.ContextID, Endpoint: r.AgentNamespace + "/" + r.AgentName, Gateway: gateway,
		OwnerId: r.OwnerID, PendingRequestId: r.ID, RequestHash: r.RequestHash,
		RuntimeSessionId: r.KagentSessionID, TaskId: r.TaskID,
	}
}

// Respond records the owner's answer to a request and returns the request's view. The answer is
// only recorded here; the dispatcher sends it. A repeat of the same action id with the same
// answer returns the same receipt and records nothing.
func (s *Service) Respond(ctx context.Context, requestID string, in api.HITLDecisionInput) (api.HITLView, error) {
	if !s.WritesEnabled {
		return api.HITLView{}, ErrDisabled
	}
	if in.ActionId == "" || len(in.ActionId) > maxActionID {
		return api.HITLView{}, fmt.Errorf("%w: action_id must be 1 to %d characters", ErrInvalid, maxActionID)
	}
	dec, err := decodeDecision(in.Response)
	if err != nil {
		return api.HITLView{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		req, err := q.LockHITLRequest(ctx, dbq.LockHITLRequestParams{ID: requestID, OwnerID: s.OwnerID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		obs, err := parseStored(req)
		if err != nil {
			return err
		}
		// Before the answer is checked, so every form of answer gets the same 409 and the reason.
		if isNested(obs) {
			return fmt.Errorf("%w: %s", ErrConflict, reasonNested)
		}
		if err := dec.validate(obs); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		hash, err := dec.hash()
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		prev, err := q.GetHITLResponse(ctx, dbq.GetHITLResponseParams{ID: in.ActionId, OwnerID: s.OwnerID})
		switch {
		case err == nil:
			if prev.RequestID != req.ID || prev.DecisionHash != hash {
				return fmt.Errorf("%w: action_id %q was used for a different answer", ErrConflict, in.ActionId)
			}
			return nil // the same action again: nothing to record
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if req.Availability != dbstate.HITLPending {
			return fmt.Errorf("%w: this request is %s and cannot be answered", ErrConflict, req.Availability)
		}
		if _, err := q.GetLiveHITLResponse(ctx, req.ID); err == nil {
			return fmt.Errorf("%w: this request already has an answer", ErrConflict)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		receipt, err := buildReceipt(req, obs, dec, in.ActionId, outboundID(in.ActionId), hash)
		if err != nil {
			return err
		}
		native, err := json.Marshal(dec.native)
		if err != nil {
			return err
		}
		if err := q.InsertHITLResponse(ctx, dbq.InsertHITLResponseParams{
			ID: in.ActionId, RequestID: req.ID, OwnerID: s.OwnerID, IdempotencyKey: "hitl-response:" + in.ActionId,
			Decision: native, DecisionHash: hash, OutboundMessageID: outboundID(in.ActionId), Receipt: receipt,
		}); err != nil {
			return err
		}
		return q.NotifyEvent(ctx, dbq.NotifyEventParams{Channel: ResponseChannel, Payload: in.ActionId})
	})
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		// Another request raced on the same action id or message id. Nothing was recorded.
		return api.HITLView{}, fmt.Errorf("%w: action_id %q is already in use", ErrConflict, in.ActionId)
	}
	if err != nil {
		return api.HITLView{}, err
	}
	return s.View(ctx, requestID)
}

// outboundID is the A2A message id of a response. It follows from the action id, so a second
// record of the same action could never mint a second message.
func outboundID(actionID string) string {
	return "mainloop-hitl-" + hashOf([]byte("hitl-response:" + actionID))[:32]
}

func buildReceipt(req dbq.HitlRequest, obs *observed, dec *decision, actionID, outbound, _ string) (json.RawMessage, error) {
	r := api.DecisionReceipt{
		ActionId: actionID, Associations: []api.VerifiedAssociation{}, Calls: []api.CallSnapshot{},
		OutboundMessageId: outbound, Outer: outerOf(req), OwnerId: req.OwnerID, RequestHash: req.RequestHash,
	}
	var err error
	switch {
	case obs.tools != nil:
		err = r.Request.FromToolApprovalRequest(*obs.tools)
		if err != nil {
			return nil, err
		}
		out := api.ToolApprovalResponseOutput{
			Approvals: make([]api.ToolApprovalOutput, 0, len(dec.tools.Approvals)),
			Type:      api.ToolApprovalResponseOutputTypeToolApprovalResponse, ReviewedContext: map[string]string{},
		}
		if dec.tools.ReviewedContext != nil {
			out.ReviewedContext = *dec.tools.ReviewedContext
		}
		for _, a := range dec.tools.Approvals {
			out.Approvals = append(out.Approvals, api.ToolApprovalOutput(a))
		}
		err = r.Response.FromToolApprovalResponseOutput(out)
		if err != nil {
			return nil, err
		}
		for _, t := range obs.tools.Tools {
			args, aerr := canonical(t.Args)
			if aerr != nil {
				return nil, aerr
			}
			approved := dec.approved[t.Id]
			callID, name := t.CallId, t.Name
			r.Calls = append(r.Calls, api.CallSnapshot{
				Approved: &approved, ArgumentsHash: hashOf(args), CallId: &callID, Leaf: leafOf(req), ToolName: &name,
			})
		}
	default:
		if err = r.Request.FromAskUserRequest(*obs.ask); err != nil {
			return nil, err
		}
		if err = r.Response.FromAskUserResponse(*dec.ask); err != nil {
			return nil, err
		}
	}
	return json.Marshal(r)
}

// View builds the contract's view of one request.
func (s *Service) View(ctx context.Context, requestID string) (api.HITLView, error) {
	q := dbq.New(s.Pool)
	req, err := q.GetHITLRequest(ctx, dbq.GetHITLRequestParams{ID: requestID, OwnerID: s.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return api.HITLView{}, ErrNotFound
	}
	if err != nil {
		return api.HITLView{}, err
	}
	obs, err := parseStored(req)
	if err != nil {
		return api.HITLView{}, err
	}
	proj := api.HITLProjection{
		Associations: []api.VerifiedAssociation{}, Availability: api.HITLProjectionAvailability(req.Availability),
		Id: req.ID, Leaves: []api.LeafIdentity{leafOf(req)}, Outer: outerOf(req), OwnerId: req.OwnerID,
		UnavailableReason: req.UnavailableReason,
	}
	if obs.tools != nil {
		err = proj.Payload.FromToolApprovalRequest(*obs.tools)
	} else {
		err = proj.Payload.FromAskUserRequest(*obs.ask)
	}
	if err != nil {
		return api.HITLView{}, err
	}
	view := api.HITLView{
		Request: proj, RouteRequestId: req.ID, WritesEnabled: s.WritesEnabled,
		UnavailableReason: req.UnavailableReason, Context: s.contextOf(ctx, req),
	}
	live := false
	resp, err := q.GetLatestHITLResponse(ctx, dbq.GetLatestHITLResponseParams{RequestID: req.ID, OwnerID: s.OwnerID})
	switch {
	case err == nil:
		state := string(resp.State)
		view.TransportState = &state
		// A response kagent never took can be given again, so it is not shown as the answer; the
		// state and the reason stay.
		if resp.State != dbstate.HITLRejectedTransport {
			var receipt api.DecisionReceipt
			if err := json.Unmarshal(resp.Receipt, &receipt); err != nil {
				return api.HITLView{}, fmt.Errorf("read receipt: %w", err)
			}
			view.Response = &receipt
			live = true
		}
		if resp.Detail != nil && resp.State != dbstate.HITLAccepted {
			detail := clip(*resp.Detail, 2000)
			view.TransportDetail = &detail
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return api.HITLView{}, err
	}
	nested := isNested(obs)
	view.Answerable = s.WritesEnabled && req.Availability == dbstate.HITLPending && !live && !nested
	if nested && view.UnavailableReason == nil {
		reason := reasonNested
		view.UnavailableReason = &reason
	}
	return view, nil
}

func (s *Service) contextOf(ctx context.Context, r dbq.HitlRequest) api.HITLContext {
	c, err := dbq.New(s.Pool).GetHITLContext(ctx, dbq.GetHITLContextParams{ID: r.SessionID, UserID: r.OwnerID})
	if err != nil {
		return api.HITLContext{}
	}
	sid := r.SessionID
	out := api.HITLContext{SessionId: &sid, Title: &c.Title, Provider: c.Provider, ProjectId: c.ProjectID, ProjectName: c.ProjectName}
	if role := string(c.Role); role != "" {
		out.Role = &role
	}
	return out
}

// SessionRequests lists the ids of a session's requests, oldest first, newest 100.
func (s *Service) SessionRequests(ctx context.Context, sessionID string) ([]string, error) {
	q := dbq.New(s.Pool)
	if _, err := q.GetHITLContext(ctx, dbq.GetHITLContextParams{ID: sessionID, UserID: s.OwnerID}); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	return q.ListSessionHITLRequests(ctx, dbq.ListSessionHITLRequestsParams{SessionID: sessionID, OwnerID: s.OwnerID, Limit: 100})
}

// Status is the observer's status.
func (s *Service) Status(ctx context.Context) (api.HITLObserverStatus, error) {
	q := dbq.New(s.Pool)
	stats, err := q.HITLObserverStats(ctx, s.OwnerID)
	if err != nil {
		return api.HITLObserverStatus{}, err
	}
	diags, err := q.ListHITLDiagnostics(ctx, s.OwnerID)
	if err != nil {
		return api.HITLObserverStatus{}, err
	}
	out := api.HITLObserverStatus{PendingTaskCount: int(stats.Pending), Diagnostics: make([]api.HITLObserverDiagnostic, 0, len(diags))}
	if stats.Pending > 0 {
		t := stats.OldestCheck
		out.OldestTaskCheck = &t
	}
	for _, d := range diags {
		out.Diagnostics = append(out.Diagnostics, api.HITLObserverDiagnostic{
			Detail: d.Detail, ObservedAt: d.ObservedAt, RuntimeSessionId: d.RuntimeSessionID,
		})
	}
	return out, nil
}

// invalidMessage strips the sentinel prefix from a wrapped error for the response body.
func invalidMessage(err error) string {
	return strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": ")
}
