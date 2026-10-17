package sessions

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// The owner picks Claude or Codex for the main thread. The product session, conversation, messages,
// topics, child tasks and capacity stay; the binding's native identity changes.
//
// A switch follows the reconciler rule. It is allowed only while the main is idle. The old native
// Session is suspended first (idempotent and reversible, so a lost reply changes nothing). Then one
// transaction retires the old identity into history, revokes its credential and commits the intent:
// the target provider, its Agent, the idempotency key and the frozen create contract. Only then is
// the target created. No transaction or lock spans a kagent call. A pending target stays pending
// until its Session is confirmed ready; a failed preparation is recorded, never retried by a loop,
// and a repeated PUT checks the remote state before it tries again.

// Main providers.
const (
	ProviderClaude = config.NativeClaude
	ProviderCodex  = config.NativeCodex
)

// ErrUnavailable is a request that needs kagent, in a process that has none.
var ErrUnavailable = errors.New("sessions: kagent is not configured")

// MainProviderHistory is one retired native identity of the main thread.
type MainProviderHistory struct {
	Provider        string
	KagentSessionID *string
}

// MainProviderState is the main thread's provider selection and the truth about its native Session.
type MainProviderState struct {
	SessionID string
	// Desired is the selected provider; Effective is the one whose Session is confirmed, nil until then.
	Desired         string
	Effective       *string
	Status          string // not_started, pending, ready or failed
	KagentSessionID *string
	Error           *string
	Recoverable     bool
	History         []MainProviderHistory
}

func validProvider(p string) bool { return p == ProviderClaude || p == ProviderCodex }

// mainProvider is the first-use default.
func (s *Service) mainProvider() string {
	if validProvider(s.MainProvider) {
		return s.MainProvider
	}
	return ProviderClaude
}

// mainAgent is the Agent a new main identity of the provider is created on. A registry, when the
// service has one, decides (an operator profile may name another Agent, and a disabled profile is
// refused); otherwise the two configured main Agents do. An identity that already exists keeps the
// Agent pinned on its binding whatever the catalog says now.
func (s *Service) mainAgent(provider string) (kagent.AgentRef, error) {
	if !validProvider(provider) {
		return kagent.AgentRef{}, fmt.Errorf("%w: the provider must be claude or codex", ErrInvalid)
	}
	if s.Providers != nil {
		p, err := s.Providers.Resolve(provider, string(dbstate.RoleMain), true)
		if err != nil {
			return kagent.AgentRef{}, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		ref := p.Agents[string(dbstate.RoleMain)]
		return kagent.AgentRef{Namespace: ref.Namespace, Name: ref.Name}, nil
	}
	if provider == ProviderCodex {
		return s.MainCodexAgent, nil
	}
	return s.Agent, nil
}

// newMainAgent is mainAgent for a new identity, which must not share its Agent with the other
// provider: the Agent decides which harness runs, so one Agent cannot serve both.
func (s *Service) newMainAgent(provider string) (kagent.AgentRef, error) {
	agent, err := s.mainAgent(provider)
	if err != nil {
		return agent, err
	}
	other := ProviderClaude
	if provider == ProviderClaude {
		other = ProviderCodex
	}
	if o, err := s.mainAgent(other); err == nil && o == agent {
		return agent, fmt.Errorf("%w: agent_conflict: both providers name the main Agent %s", ErrInvalid, agent.Name)
	}
	return agent, nil
}

// checkMainReady refuses a message to a main whose target provider is not confirmed: the Session it
// would be sent to may not exist yet. A report that meets this waits in its queue and is sent after.
func checkMainReady(ctx context.Context, q *dbq.Queries, sessionID string) error {
	b, err := q.GetAgentBinding(ctx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get binding: %w", err)
	}
	if b.MainPending {
		return conflictCode(codePending, "The main thread is switching to %s; messages wait until it is ready.", b.Kind)
	}
	return nil
}

// agentOf is the Agent a main binding runs on: the pinned one, else its provider's configured one.
func (s *Service) agentOf(b dbq.GetMainBindingRow) kagent.AgentRef {
	if b.KagentAgentNamespace != nil && b.KagentAgentName != nil {
		return kagent.AgentRef{Namespace: *b.KagentAgentNamespace, Name: *b.KagentAgentName}
	}
	// A first-use main is not pinned: it follows the configured Agent of its provider.
	kind := b.Kind
	if ref, err := s.bindingAgent(&kind, dbstate.RoleMain); err == nil {
		return ref
	}
	return s.Agent
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func ptrStr(s string) *string { return &s }

func providerState(b dbq.GetMainBindingRow, history []dbq.MainProviderHistory) MainProviderState {
	st := MainProviderState{
		SessionID: b.SessionID, Desired: b.Kind, KagentSessionID: b.KagentSessionID,
		Error: b.MainError, Recoverable: b.MainRecoverable && b.MainPending,
		History: make([]MainProviderHistory, 0, len(history)),
	}
	switch {
	case b.MainPending && b.MainError != nil:
		st.Status = "failed"
	case b.MainPending:
		st.Status = "pending"
	case b.KagentSessionID != nil:
		st.Status = "ready"
		st.Effective = ptrStr(b.Kind)
	default:
		st.Status = "not_started"
	}
	for _, h := range history {
		st.History = append(st.History, MainProviderHistory{Provider: h.Provider, KagentSessionID: h.KagentSessionID})
	}
	return st
}

// MainProviderState reads the provider state. It creates the main thread's database rows if they
// are missing, as the sweep does, and contacts no provider.
func (s *Service) MainProviderState(ctx context.Context) (MainProviderState, error) {
	if _, err := s.EnsureMain(ctx); err != nil {
		return MainProviderState{}, err
	}
	return s.readProviderState(ctx)
}

func (s *Service) readProviderState(ctx context.Context) (MainProviderState, error) {
	q := dbq.New(s.Pool)
	b, err := q.GetMainBinding(ctx, s.OwnerID)
	if err != nil {
		return MainProviderState{}, fmt.Errorf("get main binding: %w", err)
	}
	return s.stateOf(ctx, q, b)
}

func (s *Service) stateOf(ctx context.Context, q *dbq.Queries, b dbq.GetMainBindingRow) (MainProviderState, error) {
	h, err := q.ListMainProviderHistory(ctx, b.SessionID)
	if err != nil {
		return MainProviderState{}, fmt.Errorf("list provider history: %w", err)
	}
	return providerState(b, h), nil
}

// SelectMainProvider selects the main thread's provider. Selecting the current one changes nothing,
// and repeating a pending selection recovers its create contract. restartFailed admits a fresh
// identity for a pending target that is recoverable. The result is the state afterwards: a switch
// that did not finish preparing is pending or failed, not an error.
func (s *Service) SelectMainProvider(ctx context.Context, provider string, restartFailed bool) (MainProviderState, error) {
	if !validProvider(provider) {
		return MainProviderState{}, fmt.Errorf("%w: the provider must be claude or codex", ErrInvalid)
	}
	if s.Kagent == nil {
		return MainProviderState{}, ErrUnavailable
	}
	if _, err := s.EnsureMain(ctx); err != nil {
		return MainProviderState{}, err
	}
	b, err := dbq.New(s.Pool).GetMainBinding(ctx, s.OwnerID)
	if err != nil {
		return MainProviderState{}, fmt.Errorf("get main binding: %w", err)
	}
	switch {
	case b.MainPending:
		err = s.resumeSwitch(ctx, b, provider, restartFailed)
	case b.Kind != provider:
		err = s.switchTo(ctx, b, provider)
	}
	if err != nil {
		return MainProviderState{}, err
	}
	return s.readProviderState(ctx)
}

// switchTo moves an idle, ready (or not yet started) main to another provider.
func (s *Service) switchTo(ctx context.Context, b dbq.GetMainBindingRow, provider string) error {
	agent, err := s.newMainAgent(provider)
	if err != nil {
		return err
	}
	if err := s.requireAgentReady(ctx, agent); err != nil {
		return err
	}
	if err := s.retireIdle(ctx, b); err != nil {
		return err
	}
	next, err := s.commitSwitch(ctx, b, provider, agent, false, nil)
	if err != nil {
		return err
	}
	if err := s.afterIntent(ctx); err != nil {
		return err
	}
	return s.prepare(ctx, next)
}

// requireAgentReady is the first thing a switch does: it reads the target Agent and refuses with 409
// when kagent has no such Agent or it has no prepared revision, before anything is suspended,
// revoked or written. A create on such an Agent would be refused anyway, but only after the old
// identity was retired.
func (s *Service) requireAgentReady(ctx context.Context, agent kagent.AgentRef) error {
	a, err := s.Kagent.GetAgent(ctx, agent)
	var se *kagent.SessionError
	switch {
	case errors.As(err, &se) && se.Code == connect.CodeNotFound:
		return conflictCode(codeAgentUnavailable, "The target Agent %s/%s does not exist.", agent.Namespace, agent.Name)
	case err != nil:
		return conflictCode(codeAgentUnavailable, "The target Agent %s/%s could not be read.", agent.Namespace, agent.Name)
	case !kagent.AgentReady(a):
		return conflictCode(codeAgentUnavailable, "The target Agent %s/%s is not ready (no prepared revision yet).", agent.Namespace, agent.Name)
	}
	return nil
}

// retireIdle establishes that the main is idle and suspends its native Session. It reads and
// suspends; it commits nothing. Anything it cannot confirm refuses the switch with 409 and
// changes nothing but possibly the Session's suspended state, which the next send reverses.
func (s *Service) retireIdle(ctx context.Context, b dbq.GetMainBindingRow) error {
	q := dbq.New(s.Pool)
	if err := s.requireNoOpenWork(ctx, q, b.SessionID); err != nil {
		return err
	}
	if b.KagentSessionID == nil {
		if b.KagentCreateRequestID != nil {
			return conflictCode(codeUnresolved, "A native Session create for the main thread is unresolved; try again once it settles.")
		}
		return nil // never started: nothing to suspend
	}
	ksid := *b.KagentSessionID
	agent := s.agentOf(b)
	sess, err := s.Kagent.GetSession(ctx, ksid)
	var se *kagent.SessionError
	switch {
	case errors.As(err, &se) && se.Code == connect.CodeNotFound:
		return nil // gone: kept in history
	case err != nil:
		return conflictCode(codeUnconfirmed, "The main thread's native state could not be confirmed idle.")
	case sessionEnded(sess):
		return nil
	}
	if st := sess.GetState(); changing(sess) ||
		(st != kagentapi.RuntimeState_RUNTIME_STATE_READY && st != kagentapi.RuntimeState_RUNTIME_STATE_SUSPENDED) {
		return conflictCode(codeUnconfirmed, "The main thread's native Session is not settled (%s); try again shortly.", st)
	}
	tasks, err := s.Kagent.ListTasks(ctx, agent, ksid)
	if err != nil {
		return conflictCode(codeUnconfirmed, "The main thread's native turns could not be confirmed finished.")
	}
	for _, t := range tasks {
		if !isTerminal(t.Status.State) {
			return conflictCode(codeBusy, "The main thread has a native turn in progress.")
		}
	}
	if sess.GetState() == kagentapi.RuntimeState_RUNTIME_STATE_SUSPENDED {
		return nil
	}
	if _, err := s.Kagent.SuspendSession(ctx, ksid); err != nil {
		return conflictCode(codeUnconfirmed, "The main thread's native Session could not be suspended.")
	}
	after, err := s.Kagent.GetSession(ctx, ksid)
	if err != nil || after.GetState() != kagentapi.RuntimeState_RUNTIME_STATE_SUSPENDED {
		return conflictCode(codeUnconfirmed, "The suspend of the main thread's native Session is unconfirmed.")
	}
	if s.AfterSuspend != nil {
		return s.AfterSuspend(ctx)
	}
	return nil
}

// requireNoOpenWork refuses while a delivery is recorded, sending, delivered, queued or uncertain,
// or a stop of the main is unconfirmed.
func (s *Service) requireNoOpenWork(ctx context.Context, q *dbq.Queries, sessionID string) error {
	c, err := q.CountMainBlockingWork(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("check the main thread is idle: %w", err)
	}
	switch {
	case c.Deliveries > 0:
		return conflictCode(codeBusy, "The main thread has open work: a message is recorded, being sent, delivered, queued or unconfirmed.")
	case c.Cancels > 0:
		return conflictCode(codeUnconfirmed, "A stop of the main thread is unconfirmed.")
	}
	return nil
}

// commitSwitch is the intent transaction: it retires the identity the caller read into history,
// revokes its credential and writes the target contract. It re-checks idleness under the send lock,
// so a message recorded since the checks refuses the switch. abandoned marks a pending target that
// was seen absent or failed; ksid, when set, is the Session id to record for it.
func (s *Service) commitSwitch(
	ctx context.Context, read dbq.GetMainBindingRow, provider string, agent kagent.AgentRef, abandoned bool, ksid *string,
) (dbq.GetMainBindingRow, error) {
	var next dbq.GetMainBindingRow
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := db.XactLock(ctx, tx, "mainloop:send:"+read.SessionID); err != nil {
			return err
		}
		q := dbq.New(tx)
		cur, err := q.LockMainBinding(ctx, read.SessionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return conflictCode(codeBusy, "The main thread changed; try again.")
		}
		if err != nil {
			return fmt.Errorf("lock main binding: %w", err)
		}
		if cur.MainPending != read.MainPending || cur.Kind != read.Kind ||
			derefStr(cur.KagentRequestID) != derefStr(read.KagentRequestID) ||
			derefStr(cur.KagentSessionID) != derefStr(read.KagentSessionID) {
			return conflictCode(codeBusy, "The main thread changed; try again.")
		}
		if !abandoned && cur.KagentSessionID == nil && cur.KagentCreateRequestID != nil {
			return conflictCode(codeUnresolved, "A native Session create for the main thread is unresolved; try again once it settles.")
		}
		if err := s.requireNoOpenWork(ctx, q, cur.SessionID); err != nil {
			return err
		}
		// A main that never started has no native identity to keep.
		if retired := cur.KagentSessionID; retired != nil || abandoned {
			if retired == nil {
				retired = ksid
			}
			if err := q.InsertMainProviderHistory(ctx, dbq.InsertMainProviderHistoryParams{
				SessionID: cur.SessionID, Provider: cur.Kind, KagentSessionID: retired,
				AgentNamespace: cur.KagentAgentNamespace, AgentName: cur.KagentAgentName,
				RequestID: cur.KagentRequestID, CreateRequestID: cur.KagentCreateRequestID,
				Model: cur.Model, Turns: cur.Turns, Abandoned: abandoned, Error: cur.MainError,
			}); err != nil {
				return fmt.Errorf("record provider history: %w", err)
			}
		}
		if _, err := agentauth.RevokeInTx(ctx, q, cur.SessionID); err != nil {
			return err
		}
		requestID := uuid.NewString()
		if _, err := q.BeginMainSwitch(ctx, dbq.BeginMainSwitchParams{
			SessionID: cur.SessionID, Kind: provider, AgentNamespace: &agent.Namespace, AgentName: &agent.Name,
			RequestID: &requestID,
		}); err != nil {
			return fmt.Errorf("record the switch intent: %w", err)
		}
		next, err = q.GetMainBinding(ctx, s.OwnerID)
		return err
	})
	return next, err
}

// resumeSwitch handles a PUT while a target is pending.
func (s *Service) resumeSwitch(ctx context.Context, b dbq.GetMainBindingRow, provider string, restart bool) error {
	other := provider != b.Kind
	switch {
	case other && !b.MainRecoverable:
		return conflictCode(codePending, "A switch to %s is pending; repeat it to recover, and choose another provider only once it is recoverable.", b.Kind)
	case !other && (!restart || !b.MainRecoverable):
		return s.prepare(ctx, b) // the same create contract
	}
	agent, err := s.newMainAgent(provider)
	if err != nil {
		return err
	}
	if err := s.requireAgentReady(ctx, agent); err != nil {
		return err
	}
	ksid, err := s.confirmAbandonable(ctx, b)
	if err != nil {
		return err
	}
	next, err := s.commitSwitch(ctx, b, provider, agent, true, ksid)
	if err != nil {
		return err
	}
	if err := s.afterIntent(ctx); err != nil {
		return err
	}
	return s.prepare(ctx, next)
}

// confirmAbandonable rechecks that a pending target is absent or settled failed or deleted before a
// fresh identity replaces it, and returns its Session id when one is known. A create whose reply was
// lost is rechecked by replaying the frozen contract: kagent answers that the request id belongs to
// a deleted Session only when nothing live holds it.
func (s *Service) confirmAbandonable(ctx context.Context, b dbq.GetMainBindingRow) (*string, error) {
	q := dbq.New(s.Pool)
	if err := s.requireNoOpenWork(ctx, q, b.SessionID); err != nil {
		return nil, err
	}
	unconfirmed := conflictCode(codeUnconfirmed, "The pending target could not be confirmed absent or failed; repeat the same selection.")
	var se *kagent.SessionError
	if b.KagentSessionID == nil {
		sess, err := s.Kagent.CreateSession(ctx, s.createRequest(b))
		switch {
		case errors.As(err, &se) && createProvenAbsent(se):
			return nil, nil
		case err != nil:
			return nil, unconfirmed
		case changing(sess):
			return nil, unconfirmed
		case sessionEnded(sess) || sess.GetState() == kagentapi.RuntimeState_RUNTIME_STATE_FAILED:
			id := sess.GetId()
			return &id, nil
		}
		// A live Session holds the contract: keep it, and let the owner recover it.
		_, _ = q.SetMainTargetSession(ctx, dbq.SetMainTargetSessionParams{
			SessionID: b.SessionID, RequestID: derefStr(b.KagentRequestID), KagentSessionID: ptrStr(sess.GetId()),
		})
		return nil, unconfirmed
	}
	sess, err := s.Kagent.GetSession(ctx, *b.KagentSessionID)
	switch {
	case errors.As(err, &se) && se.Code == connect.CodeNotFound:
		return b.KagentSessionID, nil
	case err != nil:
		return nil, unconfirmed
	case sessionEnded(sess):
		return b.KagentSessionID, nil
	case changing(sess) || sess.GetState() != kagentapi.RuntimeState_RUNTIME_STATE_FAILED:
		return nil, unconfirmed
	}
	tasks, err := s.Kagent.ListTasks(ctx, s.agentOf(b), *b.KagentSessionID)
	if err != nil {
		return nil, unconfirmed
	}
	for _, t := range tasks {
		if !isTerminal(t.Status.State) {
			return nil, conflictCode(codeBusy, "The pending target has a native turn in progress.")
		}
	}
	return b.KagentSessionID, nil
}

// createRequest is the frozen create contract of the binding's current identity. The credential
// reference is a function of the binding, so a replay carries the original's.
func (s *Service) createRequest(b dbq.GetMainBindingRow) *kagentapi.CreateSessionRequest {
	agent := s.agentOf(b)
	return &kagentapi.CreateSessionRequest{
		Agent:       &kagentapi.ResourceReference{Namespace: agent.Namespace, Name: agent.Name},
		RequestId:   derefStr(b.KagentRequestID),
		Name:        "Mainloop main thread",
		Credentials: s.Credentials.ReplayCredentialsFor(b.SessionID, derefStr(b.CredentialIdentity), s.MCPOrigin, b.Role, b.MCPGrantKind),
	}
}

// prepare creates (or finds) the pending target's Session and waits for it to be ready. It sends no
// prompt. A remote failure is recorded on the binding, bounded and redacted, and is not an error of
// the caller's request; only a database failure is returned.
func (s *Service) prepare(ctx context.Context, b dbq.GetMainBindingRow) error {
	q := dbq.New(s.Pool)
	requestID := derefStr(b.KagentRequestID)
	if _, err := q.ClearMainSwitchError(ctx, dbq.ClearMainSwitchErrorParams{SessionID: b.SessionID, RequestID: requestID}); err != nil {
		return fmt.Errorf("clear the switch error: %w", err)
	}
	fail := func(msg string, recoverable bool) error {
		detail := safeDetail(msg)
		_, err := q.FailMainSwitch(context.WithoutCancel(ctx), dbq.FailMainSwitchParams{
			SessionID: b.SessionID, RequestID: requestID, Error: &detail, Recoverable: recoverable,
		})
		return err
	}
	ksid := derefStr(b.KagentSessionID)
	var sess *kagentapi.Session
	var se *kagent.SessionError
	if ksid == "" {
		req := s.createRequest(b)
		creds, err := s.Credentials.SessionCredentialsFor(ctx, b.SessionID, derefStr(b.CredentialIdentity), s.MCPOrigin)
		if err != nil {
			return fail("the MCP credential could not be published: "+err.Error(), false)
		}
		req.Credentials = creds
		created, err := s.Kagent.CreateSession(ctx, req)
		switch {
		case errors.As(err, &se) && createHitDeleted(se):
			return fail("kagent reports that the Session of this create request was deleted", true)
		case errors.As(err, &se) && createRejectedBeforeReservation(se):
			// kagent checks the Agent's prepared revision and the create's inputs before it reserves
			// the request id, so nothing holds it: the target is proven absent.
			return fail("kagent refused the create: "+se.Error(), true)
		case err != nil:
			return fail("the native Session could not be created: "+err.Error(), false)
		}
		n, err := q.SetMainTargetSession(ctx, dbq.SetMainTargetSessionParams{
			SessionID: b.SessionID, RequestID: requestID, KagentSessionID: ptrStr(created.GetId()),
		})
		if err != nil {
			return fmt.Errorf("store the target Session: %w", err)
		}
		if n == 0 {
			return nil // a restart superseded this identity
		}
		ksid, sess = created.GetId(), created
	} else {
		got, err := s.Kagent.GetSession(ctx, ksid)
		switch {
		case errors.As(err, &se) && se.Code == connect.CodeNotFound:
			return fail("the native Session no longer exists", true)
		case err != nil:
			return fail("the native Session could not be read: "+err.Error(), false)
		}
		sess = got
	}
	if sessionEnded(sess) || sess.GetState() == kagentapi.RuntimeState_RUNTIME_STATE_FAILED {
		return fail("the native Session is "+sess.GetState().String(), true)
	}
	if err := s.ReadySession(ctx, b.SessionID, nil, ksid); err != nil {
		recoverable := false
		if got, gerr := s.Kagent.GetSession(ctx, ksid); gerr == nil {
			recoverable = sessionEnded(got) || got.GetState() == kagentapi.RuntimeState_RUNTIME_STATE_FAILED
		} else if errors.As(gerr, &se) && se.Code == connect.CodeNotFound {
			recoverable = true
		}
		return fail("the native Session is not ready: "+err.Error(), recoverable)
	}
	if _, err := q.FinishMainSwitch(ctx, dbq.FinishMainSwitchParams{
		SessionID: b.SessionID, RequestID: requestID, KagentSessionID: &ksid,
	}); err != nil {
		return fmt.Errorf("finish the switch: %w", err)
	}
	return nil
}

// afterIntent is a test seam: it runs after a switch intent commits and before the create, and an
// error it returns ends the request there, as a crash would.
func (s *Service) afterIntent(ctx context.Context) error {
	if s.AfterIntent != nil {
		return s.AfterIntent(ctx)
	}
	return nil
}

// mainAgentName is the name of the Agent a main binding runs on, for the identity strip.
func (s *Service) mainAgentName(b dbq.GetMainBindingRow) string {
	return s.agentOf(b).Name
}
