package sessions

import (
	"context"
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
	native, err := a.native(ctx, q, row)
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

func (a *SessionsAPI) native(ctx context.Context, q *dbq.Queries, row dbq.GetMainBindingRow) (*api.NativeSessionInfo, error) {
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
		AgentName: a.Svc.Agent.Name, Deliveries: deliveries, KagentSessionId: row.KagentSessionID,
		Kind: row.Kind, Model: row.Model, ParentSessionId: row.ParentSessionID,
		Role: string(row.Role), SessionId: row.SessionID, TurnInFlight: inFlight, Turns: int(row.Turns),
	}
	if row.TopicID != nil {
		if name, err := q.GetTopicName(ctx, *row.TopicID); err == nil {
			n.Topic = &name
		}
	}
	state, note := a.sessionState(ctx, row)
	n.SessionState = state
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

// sessionState reads the kagent session's state, best effort: kagent being down must not take
// the main thread view down with it.
func (a *SessionsAPI) sessionState(ctx context.Context, row dbq.GetMainBindingRow) (state *string, note string) {
	if a.Svc.Kagent == nil || row.KagentSessionID == nil || *row.KagentSessionID == "" {
		return nil, ""
	}
	ctx, cancel := context.WithTimeout(ctx, sessionStateTimeout)
	defer cancel()
	sess, err := a.Svc.Kagent.GetSession(ctx, *row.KagentSessionID)
	if err != nil {
		return nil, "kagent session unavailable: " + safeDetail(err.Error())
	}
	s := strings.ToLower(strings.TrimPrefix(sess.GetState().String(), "RUNTIME_STATE_"))
	return &s, ""
}
