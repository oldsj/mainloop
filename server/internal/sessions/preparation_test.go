package sessions_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// prep is a main thread whose kagent Session is edited on each GetSession read, to test the
// preparation observation the native route projects from that one reply.
type prep struct {
	e   *env
	sid string
	mod atomic.Pointer[func(*kagentapi.Session)]
}

func newPrep(t *testing.T) *prep {
	t.Helper()
	p := &prep{e: newEnv(t)}
	p.e.srv.TamperSession = func(proc string, s *kagentapi.Session) {
		if proc == kagent.ProcGetSession {
			if f := p.mod.Load(); f != nil {
				(*f)(s)
			}
		}
	}
	stop := p.e.run()
	id := p.e.send("hello")
	p.e.waitState(id, dbstate.DeliveryCompleted)
	stop()
	m, err := p.e.svc.EnsureMain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.sid = m.SessionID
	return p
}

// read GETs the native view with the Session edited by mod, and checks the read was observation
// only: one GetSession and no other kagent call.
func (p *prep) read(mod func(*kagentapi.Session)) *api.NativeSessionInfo {
	p.e.t.Helper()
	p.mod.Store(&mod)
	before, gets := len(p.e.srv.Requests()), p.e.srv.Count(kagent.ProcGetSession)
	resp, err := p.e.API().GetSessionNative(context.Background(), api.GetSessionNativeRequestObject{SessionId: p.sid})
	if err != nil {
		p.e.t.Fatal(err)
	}
	if n := len(p.e.srv.Requests()) - before; n != 1 || p.e.srv.Count(kagent.ProcGetSession)-gets != 1 {
		p.e.t.Fatalf("the read made %d kagent requests (%d GetSession), want exactly one GetSession", n, p.e.srv.Count(kagent.ProcGetSession)-gets)
	}
	info := api.NativeSessionInfo(resp.(api.GetSessionNative200JSONResponse))
	return &info
}

func receiptFor(s *kagentapi.Session, classification string, historical bool) *kagentapi.WorkspacePreparationReceipt {
	return &kagentapi.WorkspacePreparationReceipt{
		Original: &kagentapi.PrepareSessionWorkspaceRequest{
			SessionId: s.Id, ActionId: "action-1", CreateRequestId: "create-1", GenerationId: "gen-1",
			ActorUid: "uid-1", PreparedRevision: "rev-1", SetupProfile: "agent",
			SetupDigest: strings.Repeat("ab", 32),
		},
		ContextId: s.ContextId, Classification: classification, Historical: historical,
		ChallengeId: "challenge-fixture", ObservationSequence: 12,
		EffectObservedAt: &timestamppb.Timestamp{Seconds: 1_700_000_000, Nanos: 123_456_789},
		ObservedAt:       &timestamppb.Timestamp{Seconds: 1_700_000_001, Nanos: 987_654_321},
	}
}

func withAssociation(s *kagentapi.Session) {
	s.RuntimeAssociation = &kagentapi.RuntimeAssociation{
		GenerationId: "current-generation", Atespace: "fixture-space", ActorName: "fixture-actor",
		ActorUid: "current-uid", Phase: "active", CurrentActive: true,
	}
}

func TestReceiptClassificationsAndHistoryAreObservedOnly(t *testing.T) {
	p := newPrep(t)
	for _, c := range []struct {
		class      string
		historical bool
	}{{"confirmed", false}, {"pending", false}, {"uncertain", false}, {"definite-failure", false}, {"confirmed", true}} {
		info := p.read(func(s *kagentapi.Session) {
			s.PreparedRevision = "fixture-revision"
			s.Operation = kagentapi.RuntimeOperation_RUNTIME_OPERATION_NONE
			withAssociation(s)
			s.WorkspacePreparation = receiptFor(s, c.class, c.historical)
		})
		o := info.PreparationObservation
		if o == nil || o.State != "ready" || o.Operation != "none" || o.SessionId != *info.KagentSessionId || o.ContextId != o.SessionId ||
			!o.ContextConfirmed || o.PreparedRevision == nil || *o.PreparedRevision != "fixture-revision" || o.ReceiptStatus != "observed" {
			t.Fatalf("%v: observation %+v", c, o)
		}
		if info.SessionState == nil || *info.SessionState != "ready" {
			t.Fatalf("session_state %v", info.SessionState)
		}
		r := o.Receipt
		if r == nil || r.Classification == nil || string(*r.Classification) != c.class || r.Historical != c.historical ||
			r.ContextId != o.ContextId || r.ChallengeId == nil || *r.ChallengeId != "challenge-fixture" || r.ObservationSequence != 12 ||
			*r.EffectObservedAt != (api.NativePreparationTimestamp{Seconds: 1_700_000_000, Nanos: 123_456_789}) ||
			*r.ObservedAt != (api.NativePreparationTimestamp{Seconds: 1_700_000_001, Nanos: 987_654_321}) {
			t.Fatalf("%v: receipt %+v", c, r)
		}
		a := r.Original
		if a.SessionId != o.SessionId || *a.ActionId != "action-1" || *a.CreateRequestId != "create-1" || *a.GenerationId != "gen-1" ||
			*a.ActorUid != "uid-1" || *a.PreparedRevision != "rev-1" || string(*a.SetupProfile) != "agent" || *a.SetupDigest != strings.Repeat("ab", 32) {
			t.Fatalf("%v: original %+v", c, a)
		}
		if ra := o.RuntimeAssociation; ra == nil || ra.GenerationId != "current-generation" || ra.ActorUid != "current-uid" ||
			ra.Phase != "active" || !ra.CurrentActive {
			t.Fatalf("association %+v", ra)
		}
	}
}

func TestMissingReceiptAndAssociationAreIndependentUnknowns(t *testing.T) {
	p := newPrep(t)
	for _, receipt := range []bool{false, true} {
		for _, association := range []bool{false, true} {
			o := p.read(func(s *kagentapi.Session) {
				if association {
					withAssociation(s)
				}
				if receipt {
					s.WorkspacePreparation = receiptFor(s, "confirmed", true)
				}
			}).PreparationObservation
			want := api.NativePreparationObservationReceiptStatus("missing")
			if receipt {
				want = "observed"
			}
			if o == nil || o.ReceiptStatus != want || (o.Receipt != nil) != receipt || (o.RuntimeAssociation != nil) != association {
				t.Fatalf("receipt=%v association=%v: %+v", receipt, association, o)
			}
		}
	}
}

func TestUnavailableReceiptKeepsTheCurrentRuntimeIdentity(t *testing.T) {
	p := newPrep(t)
	for name, edit := range map[string]func(*kagentapi.WorkspacePreparationReceipt){
		"foreign session":       func(r *kagentapi.WorkspacePreparationReceipt) { r.Original.SessionId = "foreign-session" },
		"foreign context":       func(r *kagentapi.WorkspacePreparationReceipt) { r.ContextId = "foreign-context" },
		"action id too long":    func(r *kagentapi.WorkspacePreparationReceipt) { r.Original.ActionId = strings.Repeat("x", 129) },
		"action id characters":  func(r *kagentapi.WorkspacePreparationReceipt) { r.Original.ActionId = "has space" },
		"unsupported profile":   func(r *kagentapi.WorkspacePreparationReceipt) { r.Original.SetupProfile = "unsupported" },
		"invalid digest":        func(r *kagentapi.WorkspacePreparationReceipt) { r.Original.SetupDigest = "invalid" },
		"unsupported class":     func(r *kagentapi.WorkspacePreparationReceipt) { r.Classification = "unsupported" },
		"control character":     func(r *kagentapi.WorkspacePreparationReceipt) { r.ChallengeId = "bad\x07id" },
		"timestamp nanos range": func(r *kagentapi.WorkspacePreparationReceipt) { r.ObservedAt.Nanos = 1_000_000_000 },
		"timestamp year range":  func(r *kagentapi.WorkspacePreparationReceipt) { r.ObservedAt.Seconds = 253402300800 },
	} {
		info := p.read(func(s *kagentapi.Session) {
			withAssociation(s)
			s.WorkspacePreparation = receiptFor(s, "confirmed", false)
			edit(s.WorkspacePreparation)
		})
		o := info.PreparationObservation
		if o == nil || o.ReceiptStatus != "unavailable" || o.Receipt != nil || o.RuntimeAssociation == nil ||
			o.RuntimeAssociation.GenerationId != "current-generation" {
			t.Fatalf("%s: %+v", name, o)
		}
		b, _ := json.Marshal(info)
		for _, leaked := range []string{"foreign-session", "foreign-context", "unsupported", "invalid"} {
			if strings.Contains(string(b), leaked) {
				t.Fatalf("%s: %q leaked into %s", name, leaked, b)
			}
		}
	}
}

func TestAbsentReceiptValuesAndAPresentEpochTimestampAreNotInvented(t *testing.T) {
	p := newPrep(t)
	o := p.read(func(s *kagentapi.Session) {
		s.WorkspacePreparation = &kagentapi.WorkspacePreparationReceipt{
			Original:         &kagentapi.PrepareSessionWorkspaceRequest{SessionId: s.Id, CreateRequestId: "create-1"},
			ContextId:        s.ContextId,
			EffectObservedAt: &timestamppb.Timestamp{},
		}
	}).PreparationObservation
	if o == nil || o.ReceiptStatus != "observed" {
		t.Fatalf("%+v", o)
	}
	r := o.Receipt
	if r.Classification != nil || r.Original.ActionId != nil || r.Original.SetupProfile != nil || r.Original.SetupDigest != nil ||
		r.ChallengeId != nil || r.ObservationSequence != 0 || r.ObservedAt != nil ||
		r.EffectObservedAt == nil || *r.EffectObservedAt != (api.NativePreparationTimestamp{}) {
		t.Fatalf("receipt %+v", r)
	}
	b, _ := json.Marshal(r)
	for _, absent := range []string{`"action_id"`, `"classification"`, `"observed_at"`, `"challenge_id"`} {
		if strings.Contains(string(b), absent) {
			t.Fatalf("%s should be absent: %s", absent, b)
		}
	}
	if !strings.Contains(string(b), `"effect_observed_at":{"nanos":0,"seconds":0}`) {
		t.Fatalf("the present epoch timestamp was dropped: %s", b)
	}
}

func TestAPendingOperationIsRetainedWithoutWakeOrPreparation(t *testing.T) {
	p := newPrep(t)
	o := p.read(func(s *kagentapi.Session) {
		s.State = kagentapi.RuntimeState_RUNTIME_STATE_SUSPENDED
		s.Operation = kagentapi.RuntimeOperation_RUNTIME_OPERATION_RESUME
	}).PreparationObservation
	if o == nil || o.State != "suspended" || o.Operation != "resume" || o.RuntimeAssociation != nil || o.ReceiptStatus != "missing" {
		t.Fatalf("%+v", o)
	}
	// The unspecified enums are kept as they are, not turned into a guess.
	o = p.read(func(s *kagentapi.Session) {
		s.State = kagentapi.RuntimeState_RUNTIME_STATE_UNSPECIFIED
		s.Operation = kagentapi.RuntimeOperation_RUNTIME_OPERATION_UNSPECIFIED
		s.PreparedRevision = ""
	}).PreparationObservation
	if o == nil || o.State != "unspecified" || o.Operation != "unspecified" || o.PreparedRevision != nil {
		t.Fatalf("%+v", o)
	}
}

// A reply that is for another Session or context, or that has no context, is refused by the kagent
// client; a runtime field outside the contract's bounds is refused by the projection. Either way
// the observation and the state are absent and the note says why, without echoing the value.
func TestAReplyThatCannotBeProjectedLeavesNoObservation(t *testing.T) {
	p := newPrep(t)
	for name, edit := range map[string]func(*kagentapi.Session){
		"foreign session":   func(s *kagentapi.Session) { s.Id = "foreign-session"; s.ContextId = "foreign-session" },
		"foreign context":   func(s *kagentapi.Session) { s.ContextId = "foreign-context" },
		"unknown state":     func(s *kagentapi.Session) { s.State = kagentapi.RuntimeState(99) },
		"unknown operation": func(s *kagentapi.Session) { s.Operation = kagentapi.RuntimeOperation(99) },
		"revision too long": func(s *kagentapi.Session) { s.PreparedRevision = strings.Repeat("r", 129) },
		"empty association": func(s *kagentapi.Session) { s.RuntimeAssociation = &kagentapi.RuntimeAssociation{} },
		// Python withholds the whole reply when a receipt names no original request.
		"empty receipt": func(s *kagentapi.Session) {
			withAssociation(s)
			s.WorkspacePreparation = &kagentapi.WorkspacePreparationReceipt{}
		},
		"receipt without original": func(s *kagentapi.Session) {
			withAssociation(s)
			s.WorkspacePreparation = receiptFor(s, "pending", false)
			s.WorkspacePreparation.Original = nil
		},
		"association control": func(s *kagentapi.Session) { withAssociation(s); s.RuntimeAssociation.Phase = "ac\ntive" },
		"association too long": func(s *kagentapi.Session) {
			withAssociation(s)
			s.RuntimeAssociation.ActorUid = strings.Repeat("u", 257)
		},
	} {
		info := p.read(edit)
		if info.PreparationObservation != nil || info.SessionState != nil || info.Note == nil ||
			!strings.Contains(*info.Note, "kagent session unavailable") {
			t.Fatalf("%s: %+v (note %v)", name, info.PreparationObservation, info.Note)
		}
		b, _ := json.Marshal(info)
		for _, leaked := range []string{"foreign-session", "foreign-context", strings.Repeat("r", 129), strings.Repeat("u", 257)} {
			if strings.Contains(string(b), leaked) {
				t.Fatalf("%s: %q leaked into %s", name, leaked, b)
			}
		}
	}
}

// Python's test_missing_explicit_context_remains_unconfirmed: a Session that states no context is
// shown with its own id as the context, unconfirmed. Acting on it stays refused (see the kagent
// client test), and a context naming anything else is not shown.
func TestAMissingExplicitContextIsShownUnconfirmed(t *testing.T) {
	p := newPrep(t)
	o := p.read(func(s *kagentapi.Session) { s.ContextId = "" }).PreparationObservation
	if o == nil || o.SessionId != p.e.bindingKsid(p.sid) || o.ContextId != o.SessionId || o.ContextConfirmed || o.ReceiptStatus != "missing" {
		t.Fatalf("%+v", o)
	}
}

func TestNoObservationWhenGetSessionIsUnavailable(t *testing.T) {
	p := newPrep(t)
	p.e.srv.FailNext(kagent.ProcGetSession, kagenttest.Fault{Drop: true})
	p.mod.Store(ptr(func(*kagentapi.Session) {}))
	resp, err := p.e.API().GetSessionNative(context.Background(), api.GetSessionNativeRequestObject{SessionId: p.sid})
	if err != nil {
		t.Fatal(err)
	}
	info := api.NativeSessionInfo(resp.(api.GetSessionNative200JSONResponse))
	if info.PreparationObservation != nil || info.SessionState != nil || info.Note == nil {
		t.Fatalf("%+v", info)
	}
	b, _ := json.Marshal(info)
	if strings.Contains(string(b), "preparation_observation") {
		t.Fatalf("the field must be absent, not null: %s", b)
	}
}

// Nothing outside the allowlist reaches the response, whatever the Session and the receipt carry.
func TestCredentialsConfigurationAndRuntimeDataNeverSerialize(t *testing.T) {
	p := newPrep(t)
	const private = "synthetic-private-value"
	info := p.read(func(s *kagentapi.Session) {
		withAssociation(s)
		s.RuntimeAssociation.Atespace, s.RuntimeAssociation.ActorName = private, private
		s.Credentials = []*kagentapi.SessionCredential{{}}
		s.Workspace = &kagentapi.Workspace{Repo: private, Ref: private, Branch: private}
		s.DevelopmentEnvironment = &kagentapi.DevelopmentEnvironment{Image: private, PolicyIdentity: private}
		s.RuntimeComposition = &kagentapi.RuntimeComposition{CliVersion: private}
		s.Failure = &kagentapi.Failure{}
		s.Name, s.A2AAuthority = private, private
		r := receiptFor(s, "confirmed", false)
		r.Original.Workspace = &kagentapi.Workspace{Repo: private}
		r.Original.DevelopmentEnvironment = &kagentapi.DevelopmentEnvironment{Image: private, PolicyIdentity: private}
		r.Original.RuntimeComposition = &kagentapi.RuntimeComposition{CliVersion: private}
		r.RequestDigest, r.ExecutionId, r.Atespace, r.ActorName = private, private, private, private
		r.Head, r.Branch, r.TransportDigest, r.ConfigDigest, r.McpDigest, r.NativeHook = private, private, private, private, private, private
		s.WorkspacePreparation = r
	})
	if info.PreparationObservation == nil || info.PreparationObservation.ReceiptStatus != "observed" {
		t.Fatalf("%+v", info.PreparationObservation)
	}
	b, _ := json.Marshal(info.PreparationObservation)
	if strings.Contains(string(b), private) {
		t.Fatalf("a private value was serialized: %s", b)
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"credentials", "secret_ref", "workspace", "compiler_config", "config_digest", "native_hook",
		"failure_message", "runtime_composition", "development_environment", "request_digest", "execution_id", "atespace", "actor_name",
		"head", "branch", "transport_digest", "mcp_digest"} {
		if strings.Contains(string(b), `"`+banned+`"`) {
			t.Fatalf("%q is outside the allowlist: %s", banned, b)
		}
	}
}

func TestAnUnknownSessionAndAnotherOwnerNeverReachKagent(t *testing.T) {
	p := newPrep(t)
	before := len(p.e.srv.Requests())
	resp, err := p.e.API().GetSessionNative(context.Background(), api.GetSessionNativeRequestObject{SessionId: "nope"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resp.(api.GetSessionNative404JSONResponse); !ok {
		t.Fatalf("got %T", resp)
	}
	if n := len(p.e.srv.Requests()) - before; n != 0 {
		t.Fatalf("a 404 made %d kagent requests", n)
	}
}
