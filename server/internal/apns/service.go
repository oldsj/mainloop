package apns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/attention"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/redact"
)

// MaxAttempts is how many claims a delivery gets before it is recorded failed.
const MaxAttempts = 8

// Expiry is how long APNs keeps trying a phone that is offline. Later, a notification is more
// likely stale than useful: Needs you has it either way.
const Expiry = 12 * time.Hour

// Notification categories, which the app registers (docs/specs/ios-app.md).
const (
	CategoryNeedsYou = "NEEDS_YOU"
	CategoryTask     = "TASK"
)

// Table is the intent table the send loop claims from.
func Table() reconcile.Table {
	return reconcile.NewTable("apns_deliveries", string(dbstate.PushPending))
}

// Sender sends one notification; *Client in production.
type Sender interface {
	Send(ctx context.Context, r Request) (Result, error)
}

// Service fans notifications out to the owner's devices and sends them.
type Service struct {
	Pool      *pgxpool.Pool
	OwnerID   string
	Log       *slog.Logger
	Sender    Sender
	Attention *attention.Service
	// Every is the fan-out period (default 5s).
	Every time.Duration
	Now   func() time.Time
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Register adds the fan-out sweep and the send loop.
func (s *Service) Register(r *reconcile.Runner) {
	every := s.Every
	if every <= 0 {
		every = 5 * time.Second
	}
	r.AddSweep(reconcile.Sweep{Name: "apns-enqueue", Every: every, Run: s.Enqueue})
	r.Add(reconcile.Loop{Name: "apns-send", Table: Table(), Handle: s.Handle, Concurrency: 2, Interval: every})
}

// Enqueue writes a delivery for each new notification and device with a valid token. It is
// idempotent.
func (s *Service) Enqueue(ctx context.Context) error {
	if _, err := dbq.New(s.Pool).EnqueueAPNsDeliveries(ctx, s.OwnerID); err != nil {
		return fmt.Errorf("apns: enqueue: %w", err)
	}
	return nil
}

// Handle sends one claimed delivery.
func (s *Service) Handle(ctx context.Context, c reconcile.Claim) (reconcile.Outcome, error) {
	q := dbq.New(s.Pool)
	d, err := q.GetAPNsDelivery(ctx, c.ID)
	if err != nil {
		return reconcile.Retry(), err
	}
	settle := func(state dbstate.PushState, detail string) (reconcile.Outcome, error) {
		var det *string
		if detail != "" {
			det = &detail
		}
		owner := c.Owner
		rows, err := q.SettleAPNsDelivery(ctx, dbq.SettleAPNsDeliveryParams{
			State: state, Detail: det, ID: c.ID, LeaseOwner: &owner, Attempts: d.Attempts,
		})
		if err != nil {
			return reconcile.Retry(), err
		}
		if rows == 0 {
			return reconcile.Recorded(), reconcile.ErrLeaseLost
		}
		return reconcile.Recorded(), nil
	}

	switch {
	case d.RevokedAt != nil:
		return settle(dbstate.PushSuppressed, "device revoked")
	case d.Token == nil || d.Environment == nil || d.TokenInvalidAt != nil:
		return settle(dbstate.PushSuppressed, "device has no valid push token")
	case !s.now().Before(d.PushCreatedAt.Add(Expiry)):
		return settle(dbstate.PushSuppressed, "expired before it was sent")
	}
	wanted, err := q.PushStillWanted(ctx, d.PushID)
	if err != nil {
		return reconcile.Retry(), err
	}
	if !wanted {
		return settle(dbstate.PushSuppressed, "handled before it was sent")
	}
	payload, err := s.payload(ctx, q, d)
	if err != nil {
		return reconcile.Retry(), err
	}
	res, err := s.Sender.Send(ctx, Request{
		Environment: *d.Environment, DeviceToken: *d.Token, ID: uuidString(d.ApnsID.Bytes),
		CollapseID: d.PushID, Expiration: d.PushCreatedAt.Add(Expiry), Priority: 10, Payload: payload,
	})
	switch {
	case err == nil:
		return settle(dbstate.PushSent, "")
	case errors.Is(err, ErrBadToken):
		// The token is spent: stop sending to it until the phone sets a new one.
		reason := res.Reason
		if reason == "" {
			reason = fmt.Sprintf("status %d", res.Status)
		}
		if _, ierr := q.InvalidateDevicePushToken(ctx, dbq.InvalidateDevicePushTokenParams{
			DeviceID: d.DeviceID, Token: *d.Token, Reason: &reason,
		}); ierr != nil {
			return reconcile.Retry(), ierr
		}
		s.log().InfoContext(ctx, "apns: device token refused", "device", d.DeviceID, "reason", reason)
		return settle(dbstate.PushFailed, err.Error())
	case errors.Is(err, ErrRejected):
		return settle(dbstate.PushFailed, err.Error())
	case c.Attempts >= MaxAttempts:
		return settle(dbstate.PushFailed, "retries ran out: "+err.Error())
	default:
		return reconcile.Retry(), err
	}
}

// payload is the JSON the phone gets (docs/specs/push-notifications.md). It carries the title and
// what is needed, redacted, never an item's content: no command arguments, diffs or secrets.
func (s *Service) payload(ctx context.Context, q *dbq.Queries, d dbq.GetAPNsDeliveryRow) ([]byte, error) {
	type alert struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	type aps struct {
		Alert             alert  `json:"alert"`
		Sound             string `json:"sound"`
		Badge             *int   `json:"badge,omitempty"`
		ThreadID          string `json:"thread-id"`
		Category          string `json:"category"`
		InterruptionLevel string `json:"interruption-level"`
	}
	type mainloop struct {
		Type   string `json:"type"`
		ItemID string `json:"item_id,omitempty"`
		TaskID string `json:"task_id,omitempty"`
		Href   string `json:"href"`
	}
	level := "active"
	if d.Priority >= 4 {
		level = "time-sensitive"
	}
	p := struct {
		APS      aps      `json:"aps"`
		Mainloop mainloop `json:"mainloop"`
	}{
		APS: aps{
			Alert: alert{Title: redact.Line(d.Title, 120), Body: redact.Line(d.Body, 300)},
			Sound: "default", InterruptionLevel: level,
		},
		Mainloop: mainloop{Type: d.PushKind, Href: d.Href},
	}
	switch dbstate.PushKind(d.PushKind) {
	case dbstate.PushAttention:
		p.APS.ThreadID, p.APS.Category = "needs-you", CategoryNeedsYou
		row, err := q.GetAttentionItem(ctx, dbq.GetAttentionItemParams{ID: d.SourceID, OwnerID: s.OwnerID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if err == nil && s.Attention != nil {
			if item, ok := s.Attention.WireItem(row); ok {
				p.Mainloop.ItemID = item.Id
				if item.TaskId != nil {
					p.Mainloop.TaskID = *item.TaskId
				}
			}
		}
	default:
		p.APS.ThreadID, p.APS.Category = "task:"+d.SourceID, CategoryTask
		p.Mainloop.TaskID = d.SourceID
	}
	if s.Attention != nil {
		// The badge is what Needs you holds now; a failed count leaves the badge as it is.
		if snap, err := s.Attention.Snapshot(ctx); err == nil {
			n := snap.Count
			p.APS.Badge = &n
		}
	}
	return json.Marshal(p)
}

func uuidString(b [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
