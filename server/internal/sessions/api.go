package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// deliveriesShown is how many recent deliveries GET /main-thread lists.
const deliveriesShown = 20

// sessionStateTimeout bounds the best-effort read of the kagent session state.
const sessionStateTimeout = 3 * time.Second

// SessionsAPI serves chat and the main thread.
type SessionsAPI struct {
	Svc *Service
}

// NewAPI builds the API. svc may be nil: the API then reads and records through a service with
// no kagent client, and sends stay recorded until a process with one picks them up.
func NewAPI(svc *Service, cfg *config.Config, pool *pgxpool.Pool, log *slog.Logger) *SessionsAPI {
	if svc == nil {
		svc = NewService(cfg, pool, nil, log)
	}
	return &SessionsAPI{Svc: svc}
}

func invalid(msg string) api.HTTPValidationError {
	return api.HTTPValidationError{Detail: &[]api.ValidationError{{
		Loc: []api.ValidationError_Loc_Item{}, Msg: msg, Type: "value_error",
	}}}
}

// SendMessage implements api.StrictServerInterface. The request has no message id, so each call
// is a new message; callers that need idempotency use Service.Send with their own id.
func (a *SessionsAPI) SendMessage(ctx context.Context, r api.SendMessageRequestObject) (api.SendMessageResponseObject, error) {
	if r.Body == nil {
		return api.SendMessage422JSONResponse(invalid("the body is required")), nil
	}
	conv := ""
	if r.Body.ConversationId != nil {
		conv = *r.Body.ConversationId
	}
	id := uuid.NewString()
	sent, err := a.Svc.Send(ctx, id, conv, r.Body.Message)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.SendMessage404JSONResponse(api.HTTPError{Detail: "Conversation not found"}), nil
	case errors.Is(err, ErrBusy):
		return api.SendMessage409JSONResponse(api.HTTPError{Detail: "A turn is already in progress for this conversation"}), nil
	case errors.Is(err, ErrInvalid):
		return api.SendMessage422JSONResponse(invalid(strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "))), nil
	case err != nil:
		return nil, err
	}
	return api.SendMessage200JSONResponse(api.ChatResponse{
		ConversationId: sent.ConversationID, DeliveryMessageId: &sent.MessageID, Pending: true,
	}), nil
}

// GetConversation implements api.StrictServerInterface. The only conversation is the main
// session's; its messages are the mirrored user and assistant messages.
func (a *SessionsAPI) GetConversation(ctx context.Context, r api.GetConversationRequestObject) (api.GetConversationResponseObject, error) {
	q := dbq.New(a.Svc.Pool)
	c, err := q.GetConversationSummary(ctx, dbq.GetConversationSummaryParams{ID: r.ConversationId, UserID: a.Svc.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return api.GetConversation404JSONResponse(api.HTTPError{Detail: "Conversation not found"}), nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.ListSessionMessages(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	msgs := make([]api.Message, 0, len(rows))
	for _, m := range rows {
		msgs = append(msgs, api.Message{
			Id: m.ID, ConversationId: m.SessionID, Role: api.MessageRole(m.Role),
			Content: m.Content, CreatedAt: m.CreatedAt,
		})
	}
	title := c.Title
	return api.GetConversation200JSONResponse(api.ConversationResponse{
		Conversation: api.Conversation{
			Id: c.ID, UserId: c.UserID, Title: &title, MessageCount: int(c.MessageCount),
			CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
		},
		Messages: msgs,
	}), nil
}

// GetMainThread implements api.StrictServerInterface. It only reads: before the reconciler has
// created the main session, session and native are null.
func (a *SessionsAPI) GetMainThread(ctx context.Context, _ api.GetMainThreadRequestObject) (api.GetMainThreadResponseObject, error) {
	q := dbq.New(a.Svc.Pool)
	topics, err := a.topics(ctx, q)
	if err != nil {
		return nil, err
	}
	info := api.MainThreadInfo{Mode: "native", Topics: topics}
	row, err := q.GetMainBinding(ctx, a.Svc.OwnerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.GetMainThread200JSONResponse(info), nil
	}
	if err != nil {
		return nil, err
	}
	native, err := a.native(ctx, q, nativeBinding{
		SessionID: row.SessionID, Kind: row.Kind, Role: row.Role, ParentSessionID: row.ParentSessionID,
		TopicID: row.TopicID, KagentSessionID: row.KagentSessionID, Model: row.Model, Turns: row.Turns,
	}, a.Svc.Agent.Name)
	if err != nil {
		return nil, err
	}
	info.ConversationId, info.SessionId, info.Native = &row.SessionID, &row.SessionID, native
	return api.GetMainThread200JSONResponse(info), nil
}

func (a *SessionsAPI) topics(ctx context.Context, q *dbq.Queries) ([]api.TopicLine, error) {
	rows, err := q.ListTopicLines(ctx, a.Svc.OwnerID)
	if err != nil {
		return nil, err
	}
	out := make([]api.TopicLine, 0, len(rows))
	for _, t := range rows {
		out = append(out, api.TopicLine{Name: t.Name, StatusLine: t.StatusLine, Pending: int(t.Pending)})
	}
	return out, nil
}

// nativeBinding is the part of a binding the identity strip shows.
type nativeBinding struct {
	SessionID       string
	Kind            string
	Role            dbstate.BindingRole
	ParentSessionID *string
	TopicID         *string
	KagentSessionID *string
	Model           *string
	Turns           int32
}

func (a *SessionsAPI) native(ctx context.Context, q *dbq.Queries, row nativeBinding, agentName string) (*api.NativeSessionInfo, error) {
	ds, err := q.ListSessionDeliveries(ctx, dbq.ListSessionDeliveriesParams{SessionID: row.SessionID, Limit: deliveriesShown})
	if err != nil {
		return nil, err
	}
	// The query returns the newest first, to cap at the newest; the view is oldest first, the order
	// the conversation reads in.
	slices.Reverse(ds)
	deliveries := make([]api.NativeDeliveryInfo, 0, len(ds))
	for _, d := range ds {
		var detail *string
		if d.Detail != nil {
			if s := safeDetail(*d.Detail); s != "" {
				detail = &s
			}
		}
		deliveries = append(deliveries, api.NativeDeliveryInfo{
			MessageId: d.ID, Source: d.Source, State: string(d.State),
			TaskId: d.TaskID, EvidenceRef: d.EvidenceRef, Detail: detail,
		})
	}
	// A turn is in flight only while it blocks a send, by the rule Send applies: an open delivery,
	// or an uncertain one still inside the hold.
	inFlight, unknown := false, false
	blocking, err := q.GetBlockingDelivery(ctx, dbq.GetBlockingDeliveryParams{
		SessionID: row.SessionID, HoldSeconds: a.Svc.opts().UncertainHold.Seconds(),
	})
	switch {
	case err == nil:
		inFlight, unknown = true, blocking.State == dbstate.DeliveryUncertain
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	n := &api.NativeSessionInfo{
		AgentName: agentName, Deliveries: deliveries, KagentSessionId: row.KagentSessionID,
		Kind: row.Kind, Model: row.Model, ParentSessionId: row.ParentSessionID,
		Role: string(row.Role), SessionId: row.SessionID, TurnInFlight: inFlight, Turns: int(row.Turns),
	}
	if row.TopicID != nil {
		if name, err := q.GetTopicName(ctx, *row.TopicID); err == nil {
			n.Topic = &name
		}
	}
	state, observation, note := a.sessionState(ctx, row)
	n.SessionState, n.PreparationObservation = state, observation
	if unknown {
		note = "delivery unknown: the last message was not replayed; new messages wait until this is settled, then check the reply and send again if needed"
	} else if n := len(deliveries); n > 0 && deliveries[n-1].State == string(dbstate.DeliveryUncertain) {
		// Past the hold and not yet settled by the delivery loop: sends are open again.
		note = "delivery unknown: the last message was not sent again; check the reply, then send again if needed"
	}
	if note != "" {
		n.Note = &note
	}
	return n, nil
}

// sessionState reads the kagent session once, best effort: kagent being down must not take the
// main thread view down with it. The state and the preparation observation both come from that one
// GetSession reply; a reply whose runtime fields cannot be projected counts as unavailable.
func (a *SessionsAPI) sessionState(ctx context.Context, row nativeBinding) (state *string, observation *api.NativePreparationObservation, note string) {
	if a.Svc.Kagent == nil || row.KagentSessionID == nil || *row.KagentSessionID == "" {
		return nil, nil, ""
	}
	ctx, cancel := context.WithTimeout(ctx, sessionStateTimeout)
	defer cancel()
	sess, err := a.Svc.Kagent.ObserveSession(ctx, *row.KagentSessionID)
	if err != nil {
		return nil, nil, "kagent session unavailable: " + safeDetail(err.Error())
	}
	observation, err = projectPreparation(sess)
	if err != nil {
		return nil, nil, "kagent session unavailable: " + err.Error()
	}
	s := strings.ToLower(strings.TrimPrefix(sess.GetState().String(), "RUNTIME_STATE_"))
	return &s, observation, ""
}

// toSession is the wire shape of a session row. needsOwner is the session's needs_owner: whether it
// has an attention item the owner can see (the attention_visible view).
func toSession(r dbq.GetSessionRowRow, needsOwner bool) api.Session {
	return api.Session{
		AgentKind: r.AgentKind, ArchivedAt: r.ArchivedAt, BaseBranch: r.BaseBranch, BranchName: r.BranchName,
		Color: r.Color, CommitSha: r.CommitSha, CompletedAt: r.CompletedAt, CreatedAt: r.CreatedAt,
		Description: r.Description, Error: r.Error, Id: r.ID, Model: r.Model, NeedsOwner: needsOwner,
		ParentSessionId: r.ParentSessionID, ProjectId: r.ProjectID, Prompt: r.Prompt, RepoUrl: r.RepoURL,
		Result: resultOf(r.Result), StartedAt: r.StartedAt, Status: api.SessionStatus(r.Status),
		Summary: r.Summary, Title: r.Title, Topic: r.Topic, UserId: r.UserID,
	}
}

func resultOf(raw json.RawMessage) *map[string]interface{} {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]interface{}
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil
	}
	return &m
}

func listRow(r dbq.ListSessionRowsRow, needsOwner bool) api.Session {
	return toSession(dbq.GetSessionRowRow(r), needsOwner)
}

// needsOwner reads one session's needs_owner from the attention visibility rule.
func (a *SessionsAPI) needsOwner(ctx context.Context, q *dbq.Queries, id string) (bool, error) {
	return q.SessionNeedsOwner(ctx, dbq.SessionNeedsOwnerParams{OwnerID: a.Svc.OwnerID, SessionID: &id})
}

// ListSessions implements api.StrictServerInterface: the owner's sessions other than the main
// thread, newest first.
func (a *SessionsAPI) ListSessions(ctx context.Context, r api.ListSessionsRequestObject) (api.ListSessionsResponseObject, error) {
	q := dbq.New(a.Svc.Pool)
	rows, err := q.ListSessionRows(ctx, dbq.ListSessionRowsParams{UserID: a.Svc.OwnerID, Status: r.Params.Status})
	if err != nil {
		return nil, err
	}
	// One read of the same view for the whole list, however many sessions it holds.
	needing, err := q.ListSessionsNeedingOwner(ctx, a.Svc.OwnerID)
	if err != nil {
		return nil, err
	}
	needs := make(map[string]bool, len(needing))
	for _, n := range needing {
		needs[n.SessionID] = true
	}
	out := make(api.ListSessions200JSONResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, listRow(row, needs[row.ID]))
	}
	return out, nil
}

// CreateSession implements api.StrictServerInterface.
func (a *SessionsAPI) CreateSession(ctx context.Context, r api.CreateSessionRequestObject) (api.CreateSessionResponseObject, error) {
	if r.Body == nil {
		return create422(invalid("the body is required"))
	}
	n := NewSession{Title: r.Body.Title, Description: r.Body.Description, Prompt: r.Body.Prompt, RepoURL: r.Body.RepoUrl}
	if r.Body.AgentKind != nil {
		n.AgentKind = *r.Body.AgentKind
	}
	id, err := a.Svc.Create(ctx, n)
	switch {
	case errors.Is(err, ErrInvalid):
		return create422(invalid(reason(err, ErrInvalid)))
	case err != nil:
		return nil, err
	}
	row, err := dbq.New(a.Svc.Pool).GetSessionRow(ctx, dbq.GetSessionRowParams{ID: id, UserID: a.Svc.OwnerID})
	if err != nil {
		return nil, err
	}
	// A session that was just made has no attention item yet.
	return api.CreateSession200JSONResponse(toSession(row, false)), nil
}

func create422(v api.HTTPValidationError) (api.CreateSessionResponseObject, error) {
	var b api.CreateSession422JSONResponseBody
	if err := b.FromHTTPValidationError(v); err != nil {
		return nil, err
	}
	return api.CreateSession422JSONResponse(b), nil
}

// GetSession implements api.StrictServerInterface.
func (a *SessionsAPI) GetSession(ctx context.Context, r api.GetSessionRequestObject) (api.GetSessionResponseObject, error) {
	row, err := dbq.New(a.Svc.Pool).GetSessionRow(ctx, dbq.GetSessionRowParams{ID: r.SessionId, UserID: a.Svc.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return api.GetSession404JSONResponse(api.HTTPError{Detail: "Session not found"}), nil
	}
	if err != nil {
		return nil, err
	}
	needs, err := a.needsOwner(ctx, dbq.New(a.Svc.Pool), row.ID)
	if err != nil {
		return nil, err
	}
	return api.GetSession200JSONResponse(toSession(row, needs)), nil
}

// GetSessionConversation implements api.StrictServerInterface.
func (a *SessionsAPI) GetSessionConversation(ctx context.Context, r api.GetSessionConversationRequestObject) (api.GetSessionConversationResponseObject, error) {
	q := dbq.New(a.Svc.Pool)
	row, err := q.GetSessionRow(ctx, dbq.GetSessionRowParams{ID: r.SessionId, UserID: a.Svc.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return api.GetSessionConversation404JSONResponse(api.HTTPError{Detail: "Session not found"}), nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.ListSessionMessages(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	needs, err := a.needsOwner(ctx, q, row.ID)
	if err != nil {
		return nil, err
	}
	msgs := make([]api.Message, 0, len(rows))
	for _, m := range rows {
		msgs = append(msgs, api.Message{
			Id: m.ID, ConversationId: m.SessionID, Role: api.MessageRole(m.Role),
			Content: m.Content, CreatedAt: m.CreatedAt,
		})
	}
	return api.GetSessionConversation200JSONResponse(api.SessionConversationResponse{Messages: msgs, Session: toSession(row, needs)}), nil
}

// GetSessionNative implements api.StrictServerInterface. A session without a native binding is 404.
func (a *SessionsAPI) GetSessionNative(ctx context.Context, r api.GetSessionNativeRequestObject) (api.GetSessionNativeResponseObject, error) {
	q := dbq.New(a.Svc.Pool)
	b, err := q.GetOwnedSessionBinding(ctx, dbq.GetOwnedSessionBindingParams{ID: r.SessionId, UserID: a.Svc.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return api.GetSessionNative404JSONResponse(api.HTTPError{Detail: "Session has no native agent binding"}), nil
	}
	if err != nil {
		return nil, err
	}
	name := a.Svc.Agent.Name
	if b.Role != dbstate.RoleMain {
		name = ""
		if ref, err := a.Svc.bindingAgentOf(b); err == nil {
			name = ref.Name
		}
	}
	info, err := a.native(ctx, q, nativeBinding{
		SessionID: b.SessionID, Kind: b.Kind, Role: b.Role, ParentSessionID: b.ParentSessionID,
		TopicID: b.TopicID, KagentSessionID: b.KagentSessionID, Model: b.Model, Turns: b.Turns,
	}, name)
	if err != nil {
		return nil, err
	}
	return api.GetSessionNative200JSONResponse(*info), nil
}

// SendSessionMessage implements api.StrictServerInterface. Like POST /chat, each call is a new
// message; at most one turn is open per session.
func (a *SessionsAPI) SendSessionMessage(ctx context.Context, r api.SendSessionMessageRequestObject) (api.SendSessionMessageResponseObject, error) {
	if r.Body == nil {
		return api.SendSessionMessage422JSONResponse(invalid("the body is required")), nil
	}
	id := uuid.NewString()
	_, err := a.Svc.SendTo(ctx, r.SessionId, id, r.Body.Message)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.SendSessionMessage404JSONResponse(api.HTTPError{Detail: "Session not found"}), nil
	case errors.Is(err, ErrBusy):
		return api.SendSessionMessage409JSONResponse(api.HTTPError{Detail: "A turn is already in progress for this session"}), nil
	case errors.Is(err, ErrConflict):
		return api.SendSessionMessage409JSONResponse(api.HTTPError{Detail: reason(err, ErrConflict)}), nil
	case errors.Is(err, ErrInvalid):
		return api.SendSessionMessage422JSONResponse(invalid(reason(err, ErrInvalid))), nil
	case err != nil:
		return nil, err
	}
	return api.SendSessionMessage200JSONResponse(api.SessionMessageResponse{MessageId: id, Status: api.SessionMessageResponseStatusOk}), nil
}

// StopTurn implements api.StrictServerInterface.
func (a *SessionsAPI) StopTurn(ctx context.Context, r api.StopTurnRequestObject) (api.StopTurnResponseObject, error) {
	out, err := a.Svc.Stop(ctx, r.SessionId)
	var unconfirmed *UnconfirmedError
	var kerr *KagentError
	switch {
	case errors.Is(err, ErrNotFound):
		return api.StopTurn404JSONResponse(api.HTTPError{Detail: "Session not found"}), nil
	case errors.Is(err, ErrConflict):
		return api.StopTurn409JSONResponse(api.HTTPError{Detail: reason(err, ErrConflict)}), nil
	case errors.As(err, &unconfirmed):
		return api.StopTurn409JSONResponse(api.HTTPError{Detail: unconfirmed.Detail}), nil
	case errors.As(err, &kerr):
		a.Svc.log().WarnContext(ctx, "stop turn failed in kagent", "session_id", r.SessionId, "error", kerr.Err)
		return api.StopTurn502JSONResponse(api.HTTPError{Detail: "Could not stop the turn: " + safeDetail(kerr.Err.Error())}), nil
	case err != nil:
		return nil, err
	}
	return api.StopTurn200JSONResponse(api.SessionStopTurnResponse{Status: api.SessionStopTurnResponseStatus(out)}), nil
}

// CancelSession implements api.StrictServerInterface.
func (a *SessionsAPI) CancelSession(ctx context.Context, r api.CancelSessionRequestObject) (api.CancelSessionResponseObject, error) {
	res, err := a.Svc.Cancel(ctx, r.SessionId)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.CancelSession404JSONResponse(api.HTTPError{Detail: "Session not found"}), nil
	case errors.Is(err, ErrConflict):
		return api.CancelSession409JSONResponse(api.HTTPError{Detail: reason(err, ErrConflict)}), nil
	case err != nil:
		return nil, err
	}
	return api.CancelSession200JSONResponse(api.SessionCancelResponse{
		Agent: api.SessionCancelResponseAgent(res.Agent), Status: api.SessionCancelResponseStatus(res.Status),
	}), nil
}

// ArchiveSession implements api.StrictServerInterface.
func (a *SessionsAPI) ArchiveSession(ctx context.Context, r api.ArchiveSessionRequestObject) (api.ArchiveSessionResponseObject, error) {
	err := a.Svc.Archive(ctx, r.SessionId)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.ArchiveSession404JSONResponse(api.HTTPError{Detail: "Session not found"}), nil
	case errors.Is(err, ErrConflict):
		return api.ArchiveSession409JSONResponse(api.HTTPError{Detail: reason(err, ErrConflict)}), nil
	case err != nil:
		return nil, err
	}
	return api.ArchiveSession200JSONResponse(api.SessionArchiveResponse{Status: api.Archived}), nil
}
