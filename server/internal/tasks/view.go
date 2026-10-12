package tasks

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// rows is everything a task view is built from.
type rows struct {
	task       dbq.Task
	attempts   []dbq.TaskAttempt
	operations []dbq.TaskOperation
	artifacts  []dbq.TaskArtifact
	reports    []dbq.TaskReport
}

// jsonObject decodes a stored JSON object. Null and empty read as an empty object.
func jsonObject(raw json.RawMessage) (map[string]any, error) {
	out := map[string]any{}
	if len(raw) == 0 || string(raw) == "null" {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("tasks: stored json: %w", err)
	}
	return out, nil
}

// environment decodes a stored environment, nil when none was accepted yet.
func environment(raw json.RawMessage) (*api.WorkspaceEnvironment, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var env api.WorkspaceEnvironment
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("tasks: stored environment: %w", err)
	}
	return &env, nil
}

func reasonOf[T ~string](r *dbstate.TaskReason) *T {
	if r == nil {
		return nil
	}
	v := T(*r)
	return &v
}

func intp(v *int32) *int {
	if v == nil {
		return nil
	}
	i := int(*v)
	return &i
}

func toTask(t dbq.Task) (api.Task, error) {
	env, err := environment(t.AcceptedEnvironment)
	if err != nil {
		return api.Task{}, err
	}
	out := api.Task{
		AcceptedEnvironment: env,
		AssignedProfileId:   t.AssignedProfileID,
		Brief:               t.Brief,
		CreatedAt:           t.CreatedAt,
		CreatorBindingId:    t.CreatorBindingID,
		CurrentAttemptId:    t.CurrentAttemptID,
		Id:                  t.ID,
		Mode:                api.TaskMode(t.Mode),
		OwnerId:             t.OwnerID,
		ParentTaskId:        t.ParentTaskID,
		ProjectId:           t.ProjectID,
		ProviderConstraint:  t.ProviderConstraint,
		Reason:              reasonOf[api.TaskReason](t.Reason),
		RootTaskId:          t.RootTaskID,
		SelectionSource:     api.TaskSelectionSource(t.SelectionSource),
		Status:              api.TaskStatus(t.Status),
		Title:               t.Title,
		TopicId:             t.TopicID,
		UpdatedAt:           t.UpdatedAt,
		Version:             int(t.Version),
	}
	if t.CheckoutBranch != nil {
		c := &api.TaskCheckoutOutput{Branch: *t.CheckoutBranch}
		if t.CheckoutRef != nil {
			c.Ref = *t.CheckoutRef
		}
		if t.CheckoutDepth != nil {
			c.Depth = int(*t.CheckoutDepth)
		}
		out.Checkout = c
	}
	if t.DecisionAction != nil && t.DecisionAttemptID != nil && t.DecisionReportID != nil {
		out.OwnerDecision = &api.OwnerDecision{
			Action: *t.DecisionAction, AttemptId: *t.DecisionAttemptID, ReportId: *t.DecisionReportID,
		}
	}
	return out, nil
}

func toAttempt(a dbq.TaskAttempt) (api.TaskAttempt, error) {
	env, err := environment(a.Environment)
	if err != nil {
		return api.TaskAttempt{}, err
	}
	refs := a.EvidenceRefs
	if refs == nil {
		refs = []string{}
	}
	return api.TaskAttempt{
		AgentRef:              api.ProviderAgentRef{Name: a.AgentName, Namespace: a.AgentNamespace},
		ArchivedAt:            a.ArchivedAt,
		BindingId:             a.BindingID,
		BriefDeliveryId:       a.BriefDeliveryID,
		CheckpointRef:         a.CheckpointRef,
		ConfigurationRevision: a.ConfigurationRevision,
		CreatedAt:             a.CreatedAt,
		Depth:                 api.TaskAttemptDepth(a.Depth),
		Environment:           env,
		EvidenceRefs:          refs,
		Id:                    a.ID,
		InitialRef:            a.InitialRef,
		ManifestRef:           a.ManifestRef,
		NativeDeletedAt:       a.NativeDeletedAt,
		NativeProvider:        api.TaskAttemptNativeProvider(a.NativeProvider),
		Number:                int(a.Number),
		PredecessorId:         a.PredecessorID,
		ProfileId:             a.ProfileID,
		ResultRef:             a.ResultRef,
		RetentionHold:         a.RetentionHold,
		Role:                  api.TaskAttemptRole(a.Role),
		SessionId:             a.SessionID,
		State:                 api.TaskAttemptState(a.State),
		SuccessorId:           a.SuccessorID,
		SupersededAt:          a.SupersededAt,
		TaskId:                a.TaskID,
		UpdatedAt:             a.UpdatedAt,
		WorkspaceId:           a.WorkspaceID,
		WriterGeneration:      intp(a.WriterGeneration),
	}, nil
}

func toOperation(o dbq.TaskOperation) (api.TaskOperation, error) {
	payload, err := jsonObject(o.RequestPayload)
	if err != nil {
		return api.TaskOperation{}, err
	}
	return api.TaskOperation{
		AttemptId:         o.AttemptID,
		CheckpointRef:     o.CheckpointRef,
		CreatedAt:         o.CreatedAt,
		Id:                o.ID,
		Kind:              api.TaskOperationKind(o.Kind),
		LastConfirmedStep: api.TaskOperationLastConfirmedStep(o.LastConfirmedStep),
		ManifestRef:       o.ManifestRef,
		OwnerId:           o.OwnerID,
		PrincipalKey:      o.PrincipalKey,
		Reason:            reasonOf[api.TaskOperationReason](o.Reason),
		RequestDigest:     o.RequestDigest,
		RequestId:         o.RequestID,
		RequestPayload:    payload,
		SourceAttemptId:   o.SourceAttemptID,
		State:             api.TaskOperationState(o.State),
		TargetAttemptId:   o.TargetAttemptID,
		TaskId:            o.TaskID,
		UpdatedAt:         o.UpdatedAt,
	}, nil
}

func toReport(r dbq.TaskReport) api.TaskReport {
	refs := r.EvidenceRefs
	if refs == nil {
		refs = []string{}
	}
	return api.TaskReport{
		AttemptId: r.AttemptID, EvidenceRefs: refs, Outcome: api.TaskReportOutcome(r.Outcome),
		OwnerAction: r.OwnerAction, RequestId: r.RequestID, Summary: r.Summary, TaskId: r.TaskID,
	}
}

// eligibility is what the owner may ask for next. Retry and reassign wait for the handoff
// stack, so they are never available here; cancel is available until the task is final.
func eligibility(t dbq.Task) map[string]api.TaskEligibility {
	unavailable := func(r api.TaskEligibilityReason) api.TaskEligibility {
		return api.TaskEligibility{Reason: &r}
	}
	cancel := api.TaskEligibility{Available: true}
	if TaskFinal(t.Status) {
		cancel = unavailable(api.TaskEligibilityReason(dbstate.ReasonCancelUnavailable))
	}
	handoff := unavailable(api.TaskEligibilityReason(dbstate.ReasonHandoffUnavailable))
	return map[string]api.TaskEligibility{"cancel": cancel, "retry": handoff, "reassign": handoff}
}

// projection is what the GitHub observation last saw of the task's pull request. It is a cache:
// nothing here says the task is done. Fields the merge service has not settled stay unknown.
func projection(t dbq.Task) api.TaskProjection {
	out := api.TaskProjection{
		CiState: api.TaskProjectionCiState(t.CiState), PrState: api.TaskProjectionPrState(t.PrState),
		PendingApprovalIds: []string{}, PrUrl: t.PrUrl, PrHeadSha: t.PrHeadSha, CiHeadSha: t.CiHeadSha,
		ObservedAt: t.ProjectedAt, Branch: t.CheckoutBranch,
		Repository: t.PrRepository, PrDescription: descriptionFor(t),
	}
	out.PrNumber = intp(t.PrNumber)
	// A verified merged observation of the task's own pull request is a merged publication, whoever
	// merged it. It does not complete the task: that needs the merge service's settlement, which
	// is what sets the proposal.
	if t.PrState == prMerged {
		merged := string(api.TaskProjectionPrStateMerged)
		out.MergeState = &merged
	}
	out.MergeProposalId = t.MergeProposalID
	// Reads stay DB-only. Every CI state needs an exact head and a fresh observation; a
	// sweep preserves the collection-finish time in projected_at, including unchanged facts.
	if t.ProjectedAt == nil || t.PrHeadSha == nil || *t.PrHeadSha == "" || t.CiHeadSha == nil ||
		*t.CiHeadSha != *t.PrHeadSha {
		out.CiState = api.TaskProjectionCiStateUnknown
	} else if age := time.Since(*t.ProjectedAt); age < 0 || age > 5*time.Minute {
		out.CiState = api.TaskProjectionCiStateUnknown
	}
	return out
}

// buildViews turns loaded rows into views, one per task, in the order of tasks.
func buildViews(tasks []dbq.Task, attempts []dbq.TaskAttempt, ops []dbq.TaskOperation, arts []dbq.TaskArtifact, reports []dbq.TaskReport) ([]api.TaskView, error) {
	byID := make(map[string]*api.TaskView, len(tasks))
	views := make([]api.TaskView, len(tasks))
	for i, t := range tasks {
		task, err := toTask(t)
		if err != nil {
			return nil, err
		}
		views[i] = api.TaskView{
			Actions: eligibility(t), Artifacts: []api.TaskArtifact{}, Attempts: []api.TaskAttempt{},
			Operations: []api.TaskOperation{}, Projection: projection(t), Reports: []api.TaskReport{}, Task: task,
		}
		byID[t.ID] = &views[i]
	}
	for _, a := range attempts {
		v, err := toAttempt(a)
		if err != nil {
			return nil, err
		}
		byID[a.TaskID].Attempts = append(byID[a.TaskID].Attempts, v)
	}
	opTask := map[string]string{}
	for _, o := range ops {
		v, err := toOperation(o)
		if err != nil {
			return nil, err
		}
		if o.TaskID != nil {
			opTask[o.ID] = *o.TaskID
			byID[*o.TaskID].Operations = append(byID[*o.TaskID].Operations, v)
		}
	}
	for _, a := range arts {
		payload, err := jsonObject(json.RawMessage(a.Content))
		if err != nil {
			return nil, err
		}
		if tid, ok := opTask[a.OperationID]; ok {
			byID[tid].Artifacts = append(byID[tid].Artifacts, api.TaskArtifact{
				Id: a.ID, Kind: api.TaskArtifactKind(a.Kind), OperationId: a.OperationID, Payload: payload, Sha256: a.Sha256,
			})
		}
	}
	for _, r := range reports {
		byID[r.TaskID].Reports = append(byID[r.TaskID].Reports, toReport(r))
	}
	return views, nil
}

// hideOwnerFields blanks what only the owner may see, as Python's read_native did for agent
// bindings: operation idempotency keys and payloads, attempt evidence and internal refs, and
// checkpoint and manifest refs. History is narrowed too: an agent sees the reports of its task's
// current attempt only (no earlier attempt's report, and no handoff artifacts, because there is
// no continuation it is linked to), and no pending approval ids.
func hideOwnerFields(v *api.TaskView) {
	v.Artifacts = []api.TaskArtifact{}
	v.Projection.PendingApprovalIds = []string{}
	kept := []api.TaskReport{}
	for _, r := range v.Reports {
		if v.Task.CurrentAttemptId != nil && r.AttemptId == *v.Task.CurrentAttemptId {
			kept = append(kept, r)
		}
	}
	v.Reports = kept
	for i := range v.Operations {
		o := &v.Operations[i]
		o.PrincipalKey, o.RequestDigest, o.RequestPayload = "", "", map[string]any{}
	}
	for i := range v.Attempts {
		a := &v.Attempts[i]
		a.EvidenceRefs, a.ResultRef, a.BriefDeliveryId, a.RetentionHold = []string{}, nil, nil, nil
		a.CheckpointRef, a.ManifestRef = nil, nil
	}
}
