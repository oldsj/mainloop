// Package notify pushes notifications to the owner's phone through ntfy
// (docs/specs/push-notifications.md).
//
// It follows the reconciler rule. The enqueue sweep writes one push_notifications row per thing to
// tell, keyed by what it is about ('attention:<item>', 'task:<task>:<status>:<n>'), so repeated
// sweeps, replicas and restarts never write a second row. The send loop claims a row, rereads
// whether the owner still needs it (an item they already handled is suppressed), and posts it with
// no transaction open. ntfy has no idempotency key, so a retry first reads the topic back and
// records the row sent if an earlier attempt already arrived, instead of posting again.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// MaxAttempts is how many claims a notification gets before it is recorded failed.
const MaxAttempts = 8

// Table is the intent table the send loop claims from.
func Table() reconcile.Table {
	return reconcile.NewTable("push_notifications", string(dbstate.PushPending))
}

// Service enqueues and sends push notifications.
type Service struct {
	Pool    *pgxpool.Pool
	OwnerID string
	Log     *slog.Logger
	// URL is the ntfy base URL; Topic the topic; Token an optional access token.
	URL, Topic, Token string
	// AppURL is the web app's origin, such as https://mainloop.example.com; notification links
	// are AppURL plus the item's path.
	AppURL string
	Client *http.Client
	// Every is the enqueue period (default 5s).
	Every time.Duration
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Service) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Register adds the enqueue sweep and the send loop.
func (s *Service) Register(r *reconcile.Runner) {
	every := s.Every
	if every <= 0 {
		every = 5 * time.Second
	}
	r.AddSweep(reconcile.Sweep{Name: "push-enqueue", Every: every, Run: s.Enqueue})
	r.Add(reconcile.Loop{Name: "push-send", Table: Table(), Handle: s.Handle, Concurrency: 2, Interval: every})
}

// Enqueue writes a notification for each new Needs-you item and each root task that finished or
// got blocked. It is idempotent.
func (s *Service) Enqueue(ctx context.Context) error {
	q := dbq.New(s.Pool)
	if err := q.EnsurePushFloor(ctx); err != nil {
		return fmt.Errorf("notify: floor: %w", err)
	}
	if _, err := q.EnqueueAttentionPush(ctx, s.OwnerID); err != nil {
		return fmt.Errorf("notify: enqueue attention: %w", err)
	}
	if _, err := q.EnqueueTaskPush(ctx, s.OwnerID); err != nil {
		return fmt.Errorf("notify: enqueue tasks: %w", err)
	}
	return nil
}

// message is the JSON body ntfy publishes and returns.
type message struct {
	Topic    string `json:"topic"`
	Title    string `json:"title"`
	Message  string `json:"message"`
	Priority int    `json:"priority,omitempty"`
	Click    string `json:"click,omitempty"`
	Event    string `json:"event,omitempty"`
}

// Handle sends one claimed notification.
func (s *Service) Handle(ctx context.Context, c reconcile.Claim) (reconcile.Outcome, error) {
	q := dbq.New(s.Pool)
	n, err := q.GetPushNotification(ctx, c.ID)
	if err != nil {
		return reconcile.Retry(), err
	}
	settle := func(state dbstate.PushState, detail string) (reconcile.Outcome, error) {
		var d *string
		if detail != "" {
			d = &detail
		}
		owner := c.Owner
		rows, err := q.SettlePushNotification(ctx, dbq.SettlePushNotificationParams{
			State: state, Detail: d, ID: c.ID, LeaseOwner: &owner, Attempts: n.Attempts,
		})
		if err != nil {
			return reconcile.Retry(), err
		}
		if rows == 0 {
			return reconcile.Recorded(), reconcile.ErrLeaseLost
		}
		return reconcile.Recorded(), nil
	}

	msg := message{
		Topic: s.Topic, Title: oneLine(n.Title, 120), Message: oneLine(n.Body, 300),
		Priority: int(n.Priority), Click: s.AppURL + n.Href,
	}
	// A retry may follow a post that reached ntfy without our seeing the answer: look first.
	if c.Attempts > 1 {
		found, err := s.published(ctx, msg, n.CreatedAt)
		if err != nil {
			if c.Attempts >= MaxAttempts {
				return settle(dbstate.PushFailed, "could not confirm an earlier attempt: "+err.Error())
			}
			return reconcile.Retry(), err
		}
		if found {
			return settle(dbstate.PushSent, "found on the topic after an uncertain attempt")
		}
	}
	wanted, err := q.PushStillWanted(ctx, c.ID)
	if err != nil {
		return reconcile.Retry(), err
	}
	if !wanted {
		return settle(dbstate.PushSuppressed, "handled before it was sent")
	}
	status, err := s.post(ctx, msg)
	switch {
	case err == nil:
		return settle(dbstate.PushSent, "")
	case status >= 400 && status < 500 && status != http.StatusTooManyRequests:
		return settle(dbstate.PushFailed, err.Error())
	case c.Attempts >= MaxAttempts:
		return settle(dbstate.PushFailed, "retries ran out: "+err.Error())
	default:
		return reconcile.Retry(), err
	}
}

func (s *Service) authorize(req *http.Request) {
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
}

// post publishes msg. It returns the HTTP status (0 when there was none) and an error unless ntfy
// accepted it.
func (s *Service) post(ctx context.Context, msg message) (int, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	s.authorize(req)
	resp, err := s.client().Do(req)
	if err != nil {
		return 0, fmt.Errorf("ntfy publish: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("ntfy publish: status %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

// published reports whether the topic already holds msg, published at or after since.
func (s *Service) published(ctx context.Context, msg message, since time.Time) (bool, error) {
	u := s.URL + "/" + url.PathEscape(s.Topic) + "/json?poll=1&since=" +
		strconv.FormatInt(since.Add(-time.Minute).Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	s.authorize(req)
	resp, err := s.client().Do(req)
	if err != nil {
		return false, fmt.Errorf("ntfy poll: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return false, fmt.Errorf("ntfy poll: status %d", resp.StatusCode)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 8<<20))
	for {
		var m message
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			return false, nil
		} else if err != nil {
			return false, fmt.Errorf("ntfy poll: %w", err)
		}
		if (m.Event == "" || m.Event == "message") && m.Title == msg.Title && m.Message == msg.Message && m.Click == msg.Click {
			return true, nil
		}
	}
}

// oneLine folds whitespace and cuts s to max runes.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}
