package hitl

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/kagent"
)

// MergeRuntime reads trusted Session and Agent identity; native tool arguments cannot supply it.
// The production client adapter exposes these methods from the shared kagent client.
type MergeRuntime interface {
	SessionAgent(context.Context, string) (kagent.AgentRef, error)
	AgentTemplate(context.Context, kagent.AgentRef) (string, error)
}

var _ MergeRuntime = clientAdapter{}

// mergeContexts reads bounded immutable presentations, never GitHub facts or full detail pages.
// A mapping requires the request's current runtime and exactly one template/provider configuration.
func (s *Service) mergeContexts(ctx context.Context, r dbq.HitlRequest, o *observed) ([]api.VerifiedMergeContext, error) {
	out := []api.VerifiedMergeContext{}
	if o.tools == nil || o.tools.Nested != nil || len(s.MergeConfigs) == 0 {
		return out, nil
	}
	names := map[string]bool{}
	for _, cfg := range s.MergeConfigs {
		names[cfg.PublicName()] = true
	}
	needed := false
	for _, tool := range o.tools.Tools {
		needed = needed || names[tool.Name]
	}
	if !needed {
		return out, nil
	}
	var template string
	trusted := false
	if s.MergeRuntime != nil {
		readCtx, cancel := context.WithTimeout(ctx, s.opts().ReadTimeout)
		defer cancel()
		ref := s.agentOf(r.AgentNamespace, r.AgentName)
		live, err := s.MergeRuntime.SessionAgent(readCtx, r.KagentSessionID)
		if err == nil && live == ref {
			template, err = s.MergeRuntime.AgentTemplate(readCtx, ref)
			trusted = err == nil && template != ""
		}
	}
	for _, tool := range o.tools.Tools {
		if !names[tool.Name] {
			continue
		}
		reason := "Merge template mapping is unavailable."
		id := tool.Id
		context := api.VerifiedMergeContext{ToolId: &id, Availability: api.VerifiedMergeContextAvailabilityUnavailable,
			FreshnessReason: &reason, ProtectedMatches: []string{}}
		pid, _ := tool.Args["proposal_id"].(string)
		rid, _ := tool.Args["request_id"].(string)
		if !trusted || pid == "" || rid == "" || len(tool.Args) != 2 {
			out = append(out, context)
			continue
		}
		row, err := dbq.New(s.Pool).GetHITLMergeProposal(ctx, dbq.GetHITLMergeProposalParams{
			ID: pid, OwnerID: r.OwnerID, SessionID: r.SessionID, KagentSessionID: &r.KagentSessionID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			reason = "No merge proposal matches this owner, project, session and runtime."
			out = append(out, context)
			continue
		}
		if err != nil {
			return nil, err
		}
		matches := []config.MergeConfiguration{}
		for _, cfg := range s.MergeConfigs {
			if cfg.TemplateName == template && cfg.Provider == row.Provider {
				matches = append(matches, cfg)
			}
		}
		if len(matches) != 1 || matches[0].PublicName() != tool.Name {
			out = append(out, context)
			continue
		}
		var summary api.MergeSummary
		if err := json.Unmarshal(row.Presentation, &summary); err != nil {
			return nil, err
		}
		context.ProposalId, context.Summary, context.SummaryDigest = &row.ProposalID, &summary, &row.SummaryDigest
		context.Repository, context.PrNumber = summary.Repository, summary.PrNumber
		context.Head, context.HeadSha, context.Base, context.BaseSha = summary.Head, summary.HeadSha, summary.Base, summary.BaseSha
		context.ProtectedMatches = summary.ProtectedMatches
		state := string(row.State)
		context.State = &state
		if row.Deadline != nil {
			deadline := row.Deadline.UTC().Format(time.RFC3339)
			context.Deadline = &deadline
		}
		context.Stale = r.Availability != dbstate.HITLPending || row.State != dbstate.MergePrepared || (row.Deadline != nil && !row.Deadline.After(time.Now()))
		switch {
		case context.Stale:
			context.Availability = api.VerifiedMergeContextAvailabilityStale
			reason = "This request or merge proposal is no longer awaiting approval."
		case summary.Availability != api.MergeSummaryAvailabilityReady:
			reason = "The captured merge evidence is unavailable."
		default:
			context.Availability = api.VerifiedMergeContextAvailabilityReady
			context.FreshnessReason = nil
		}
		out = append(out, context)
	}
	return out, nil
}

// bindMerges is observer work, so a browser never has to open a card to create proposal attention.
// All runtime reads finish before the transaction. Repeated associations and sweeps are no-ops.
func (s *Service) bindMerges(ctx context.Context, r dbq.HitlRequest) error {
	o, err := parseStored(r)
	if err != nil {
		return err
	}
	contexts, err := s.mergeContexts(ctx, r, o)
	if err != nil || len(contexts) == 0 {
		return err
	}
	return events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		changed := false
		for _, c := range contexts {
			if c.ProposalId == nil || c.ToolId == nil {
				continue
			}
			n, err := q.InsertHITLMergeTool(ctx, dbq.InsertHITLMergeToolParams{RequestID: r.ID, ToolID: *c.ToolId, ProposalID: *c.ProposalId})
			if err != nil {
				return nil, err
			}
			changed = changed || n > 0
		}
		for _, step := range []func(context.Context, string) ([]string, error){q.OpenMergeProposalAttention, q.ResolveMergeProposalAttention} {
			ids, err := step(ctx, r.OwnerID)
			if err != nil {
				return nil, err
			}
			changed = changed || len(ids) > 0
		}
		if !changed {
			return nil, nil
		}
		return inboxDraft(ctx, q, r.OwnerID, attentionID(r.ID))
	})
}
