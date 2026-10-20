package sessions

import (
	"errors"
	"regexp"
	"unicode/utf8"

	"github.com/oldsj/mainloop/server/internal/api"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1alpha1"
)

// The preparation observation is a bounded projection of the kagent Session that GetSession already
// returned for the native identity strip. It reads no second RPC, refreshes nothing and grants no
// authority. Every value is checked against the contract's bounds; a runtime field that fails makes
// the whole observation unavailable, a receipt that fails makes only the receipt unavailable.

var (
	errUnprojectable = errors.New("preparation observation cannot be projected")
	actionIDPattern  = regexp.MustCompile(`^[A-Za-z0-9:._-]+$`)
	digestPattern    = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

const (
	observationIdentityMax = 256
	preparationIdentityMax = 128
	minTimestampSeconds    = -62135596800
	maxTimestampSeconds    = 253402300799
)

// identityOK reports whether v is 1..limit characters without control characters.
func identityOK(v string, limit int) bool {
	n := utf8.RuneCountInString(v)
	if n < 1 || n > limit {
		return false
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// optional returns nil for an empty value, else a pointer to it when ok(v); bad is set when a
// non-empty value fails ok.
func optional(v string, ok func(string) bool, bad *bool) *string {
	if v == "" {
		return nil
	}
	if !ok(v) {
		*bad = true
		return nil
	}
	return &v
}

func preparationIdentity(v string) bool { return identityOK(v, preparationIdentityMax) }

func oneOf(set ...string) func(string) bool {
	return func(v string) bool {
		for _, s := range set {
			if s == v {
				return true
			}
		}
		return false
	}
}

func preparationTime(ts interface {
	GetSeconds() int64
	GetNanos() int32
}, present bool, bad *bool) *api.NativePreparationTimestamp {
	if !present {
		return nil
	}
	s, n := ts.GetSeconds(), ts.GetNanos()
	if s < minTimestampSeconds || s > maxTimestampSeconds || n < 0 || n >= 1_000_000_000 {
		*bad = true
		return nil
	}
	return &api.NativePreparationTimestamp{Seconds: s, Nanos: int(n)}
}

// stateName and operationName map kagent's runtime enums to the contract's names; a value this
// client does not know is not projected.
func stateName(s kagentapi.RuntimeState) (api.NativePreparationObservationState, bool) {
	switch s {
	case kagentapi.RuntimeState_RUNTIME_STATE_UNSPECIFIED:
		return "unspecified", true
	case kagentapi.RuntimeState_RUNTIME_STATE_CREATING:
		return "creating", true
	case kagentapi.RuntimeState_RUNTIME_STATE_READY:
		return "ready", true
	case kagentapi.RuntimeState_RUNTIME_STATE_SUSPENDED:
		return "suspended", true
	case kagentapi.RuntimeState_RUNTIME_STATE_FAILED:
		return "failed", true
	case kagentapi.RuntimeState_RUNTIME_STATE_DELETING:
		return "deleting", true
	case kagentapi.RuntimeState_RUNTIME_STATE_DELETED:
		return "deleted", true
	}
	return "", false
}

func operationName(o kagentapi.RuntimeOperation) (api.NativePreparationObservationOperation, bool) {
	switch o {
	case kagentapi.RuntimeOperation_RUNTIME_OPERATION_UNSPECIFIED:
		return "unspecified", true
	case kagentapi.RuntimeOperation_RUNTIME_OPERATION_CREATE:
		return "create", true
	case kagentapi.RuntimeOperation_RUNTIME_OPERATION_SUSPEND:
		return "suspend", true
	case kagentapi.RuntimeOperation_RUNTIME_OPERATION_RESUME:
		return "resume", true
	case kagentapi.RuntimeOperation_RUNTIME_OPERATION_DELETE:
		return "delete", true
	case kagentapi.RuntimeOperation_RUNTIME_OPERATION_NONE:
		return "none", true
	}
	return "", false
}

// projectPreparation projects explicit safe fields only; it never serializes the Session, the
// original request or anything outside the contract's allowlist.
func projectPreparation(s *kagentapi.Session) (*api.NativePreparationObservation, error) {
	state, okState := stateName(s.GetState())
	op, okOp := operationName(s.GetOperation())
	bad := false
	// A Session that states no context id is shown with its own id as the context, unconfirmed.
	context := s.GetContextId()
	confirmed := context != ""
	if !confirmed {
		context = s.GetId()
	}
	obs := &api.NativePreparationObservation{
		SessionId: s.GetId(), ContextId: context, ContextConfirmed: confirmed,
		State: state, Operation: op,
		PreparedRevision: optional(s.GetPreparedRevision(), preparationIdentity, &bad),
	}
	if !okState || !okOp || bad ||
		!identityOK(obs.SessionId, observationIdentityMax) || !identityOK(obs.ContextId, observationIdentityMax) {
		return nil, errUnprojectable
	}
	if ra := s.GetRuntimeAssociation(); ra != nil {
		if !identityOK(ra.GetGenerationId(), observationIdentityMax) || !identityOK(ra.GetActorUid(), observationIdentityMax) ||
			!identityOK(ra.GetPhase(), observationIdentityMax) {
			return nil, errUnprojectable
		}
		obs.RuntimeAssociation = &api.NativeRuntimeAssociationInfo{
			GenerationId: ra.GetGenerationId(), ActorUid: ra.GetActorUid(), Phase: ra.GetPhase(),
			CurrentActive: ra.GetCurrentActive(),
		}
	}
	saved := s.GetWorkspacePreparation()
	obs.ReceiptStatus = api.NativePreparationObservationReceiptStatus("missing")
	if saved == nil {
		return obs, nil
	}
	// A receipt that names no original request is malformed, not merely foreign: the Python client
	// refuses the whole reply, so no runtime field is shown beside it.
	if saved.GetOriginal() == nil {
		return nil, errUnprojectable
	}
	// A receipt for another Session or context is neither this Session's evidence nor
	// owner-visible data, and one that does not fit the bounds is not shown either.
	obs.ReceiptStatus = "unavailable"
	if saved.GetOriginal().GetSessionId() != s.GetId() || saved.GetContextId() != obs.ContextId {
		return obs, nil
	}
	if receipt, ok := projectReceipt(saved); ok {
		obs.Receipt, obs.ReceiptStatus = receipt, "observed"
	}
	return obs, nil
}

func projectReceipt(saved *kagentapi.WorkspacePreparationReceipt) (*api.NativePreparationReceiptInfo, bool) {
	o := saved.GetOriginal()
	bad := false
	action := api.NativePreparationActionInfo{
		SessionId:        o.GetSessionId(),
		ActionId:         optional(o.GetActionId(), func(v string) bool { return len(v) <= preparationIdentityMax && actionIDPattern.MatchString(v) }, &bad),
		CreateRequestId:  optional(o.GetCreateRequestId(), preparationIdentity, &bad),
		GenerationId:     optional(o.GetGenerationId(), preparationIdentity, &bad),
		ActorUid:         optional(o.GetActorUid(), preparationIdentity, &bad),
		PreparedRevision: optional(o.GetPreparedRevision(), preparationIdentity, &bad),
	}
	if p := optional(o.GetSetupProfile(), oneOf("supervisor", "child", "agent"), &bad); p != nil {
		profile := api.NativePreparationActionInfoSetupProfile(*p)
		action.SetupProfile = &profile
	}
	action.SetupDigest = optional(o.GetSetupDigest(), digestPattern.MatchString, &bad)
	receipt := &api.NativePreparationReceiptInfo{
		Original: action, ContextId: saved.GetContextId(), Historical: saved.GetHistorical(),
		ChallengeId:         optional(saved.GetChallengeId(), preparationIdentity, &bad),
		ObservationSequence: saved.GetObservationSequence(),
		EffectObservedAt:    preparationTime(saved.GetEffectObservedAt(), saved.GetEffectObservedAt() != nil, &bad),
		ObservedAt:          preparationTime(saved.GetObservedAt(), saved.GetObservedAt() != nil, &bad),
	}
	if c := optional(saved.GetClassification(), oneOf("pending", "uncertain", "confirmed", "definite-failure"), &bad); c != nil {
		class := api.NativePreparationReceiptInfoClassification(*c)
		receipt.Classification = &class
	}
	if bad || !identityOK(action.SessionId, observationIdentityMax) || !identityOK(receipt.ContextId, observationIdentityMax) {
		return nil, false
	}
	return receipt, true
}
