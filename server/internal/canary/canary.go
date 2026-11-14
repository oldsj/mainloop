// Package canary runs one cheap synthetic turn through a running Mainloop and reports which step
// failed or was slow (docs/specs/canary.md). It is the one-shot body of `mainloop canary`, meant
// for a CronJob: it keeps no state of its own; the session is found again by its title among the
// hidden sessions.
//
// The turn is the cheapest one the product can run: a one-word prompt on the smallest model (set
// at create, so even the first turn uses it), in one long-lived hidden session, so each run is a
// warm wake of an idle session (the path the owner's own sessions take) rather than a cold start.
// The session is hidden from the owner's sessions list, has no coordination grant (it never reports
// to the main thread) and starts no task. It is recreated only when it breaks.
package canary

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/api"
)

// TitlePrefix starts the title of every canary session; the date follows.
const TitlePrefix = "Mainloop canary "

// Prompt is the whole synthetic turn.
const Prompt = "ping"

// Options configure one run against one Mainloop.
type Options struct {
	// Name labels the target in reports, such as "prod".
	Name string
	// API is the owner API base URL, without a trailing slash.
	API string
	// Host is an optional Host header for the API.
	Host string
	// Model is the model override set on each new canary session.
	Model string
	// Slow is the reply time above which the run reports slow; Deadline the time after which it
	// gives up waiting.
	Slow, Deadline time.Duration
	// Poll is the pause between conversation reads.
	Poll time.Duration
	// Request bounds each HTTP request.
	Request time.Duration
	Client  *http.Client
	// Now is the clock (tests).
	Now func() time.Time
}

// Result is the outcome of one run.
type Result struct {
	Target string
	// Step names the step that failed or was slow; empty when the run passed.
	Step string
	// Err is set when Step failed.
	Err error
	// Slow is set when every step passed but the reply took longer than Options.Slow.
	Slow bool
	// Created is set when the run had to create the canary session (a cold start).
	Created bool
	Elapsed time.Duration
	Session string
}

// OK reports whether the run passed in time.
func (r Result) OK() bool { return r.Err == nil && !r.Slow }

// String is the one-line report.
func (r Result) String() string {
	switch {
	case r.Err != nil:
		return fmt.Sprintf("fail %s step=%s session=%s: %v", r.Target, r.Step, r.Session, r.Err)
	case r.Slow:
		return fmt.Sprintf("slow %s reply in %s session=%s%s", r.Target, r.Elapsed.Round(time.Second), r.Session, r.cold())
	default:
		return fmt.Sprintf("ok %s reply in %s session=%s%s", r.Target, r.Elapsed.Round(100*time.Millisecond), r.Session, r.cold())
	}
}

func (r Result) cold() string {
	if r.Created {
		return " (new session, cold start)"
	}
	return ""
}

// The steps, in order.
const (
	StepList    = "list-sessions"
	StepCreate  = "create-session"
	StepSend    = "send-message"
	StepReply   = "await-reply"
	StepCleanup = "cleanup"
)

type runner struct {
	o Options
}

// usable reports whether a canary session can take the next ping.
func usable(s api.Session) bool {
	switch s.Status {
	case api.SessionStatusFailed, api.SessionStatusCancelled, api.SessionStatusCompleted, api.SessionStatusSuperseded:
		return false
	}
	return s.ArchivedAt == nil
}

// Run does one canary turn. It reuses the one hidden canary session, so a run is a warm wake of an
// idle session, and creates it (hidden, on the canary model) only when none is usable. A turn that
// gets no reply is stopped, so nothing is left running to stall.
func Run(ctx context.Context, o Options) (res Result) {
	if o.Client == nil {
		o.Client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	r := runner{o: o}
	res = Result{Target: o.Name}
	fail := func(step string, err error) Result {
		res.Step, res.Err = step, err
		return res
	}

	var sessions []api.Session
	if err := r.call(ctx, http.MethodGet, "/sessions?hidden=true", nil, &sessions); err != nil {
		return fail(StepList, err)
	}
	// The list is newest first: keep the newest usable canary session, retire the rest.
	var current *api.Session
	var retire []string
	for i, s := range sessions {
		if !strings.HasPrefix(s.Title, TitlePrefix) {
			continue
		}
		if current == nil && usable(s) {
			current = &sessions[i]
		} else {
			retire = append(retire, s.Id)
		}
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*o.Request)
		defer cancel()
		for _, id := range retire {
			if err := r.retire(cctx, id); err != nil && res.Err == nil {
				res = fail(StepCleanup, err)
			}
		}
	}()

	start := o.Now()
	messageID := ""
	if current == nil {
		body := map[string]any{
			"title":       TitlePrefix + start.UTC().Format("2006-01-02 15:04"),
			"description": "Synthetic turn that checks Mainloop end to end (docs/specs/canary.md). Hidden from the sessions list.",
			"prompt":      Prompt,
			"hidden":      true,
		}
		if o.Model != "" {
			body["model"] = o.Model
		}
		var created api.Session
		if err := r.call(ctx, http.MethodPost, "/sessions", body, &created); err != nil {
			return fail(StepCreate, err)
		}
		res.Session, res.Created = created.Id, true
	} else {
		res.Session = current.Id
		messageID = uuid.NewString()
		var sent api.SessionMessageResponse
		if err := r.call(ctx, http.MethodPost, "/sessions/"+current.Id+"/message",
			map[string]any{"message": Prompt, "message_id": messageID}, &sent); err != nil {
			return fail(StepSend, err)
		}
		if sent.Status != api.SessionMessageResponseStatusOk {
			// Queued or steered behind an open turn: an earlier canary turn never ended.
			r.stop(ctx, current.Id)
			return fail(StepSend, fmt.Errorf("message %s: an earlier turn is still open; stopped it", sent.Status))
		}
	}

	err := r.awaitReply(ctx, res.Session, messageID)
	res.Elapsed = o.Now().Sub(start)
	if err != nil {
		r.stop(ctx, res.Session)
		return fail(StepReply, err)
	}
	res.Slow = res.Elapsed > o.Slow
	return res
}

// stop ends the session's open turn, best effort: a failed stop shows up as the next run's
// send-message failure.
func (r runner) stop(ctx context.Context, id string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.o.Request)
	defer cancel()
	_ = r.call(cctx, http.MethodPost, "/sessions/"+id+"/stop-turn", map[string]any{}, nil)
}

// retire cancels and archives a canary session that is broken or superseded. One that already
// ended answers the cancel with 409; the archive is what matters.
func (r runner) retire(ctx context.Context, id string) error {
	if err := r.call(ctx, http.MethodPost, "/sessions/"+id+"/cancel", map[string]any{}, nil); err != nil &&
		!strings.HasSuffix(err.Error(), "status 409") {
		return err
	}
	return r.call(ctx, http.MethodPost, "/sessions/"+id+"/archive", map[string]any{}, nil)
}

// awaitReply waits for an assistant message after the user message messageID, or after the first
// user message when messageID is empty (a new session).
func (r runner) awaitReply(ctx context.Context, session, messageID string) error {
	ctx, cancel := context.WithTimeout(ctx, r.o.Deadline)
	defer cancel()
	var lastErr error
	for {
		var conv api.SessionConversationResponse
		err := r.call(ctx, http.MethodGet, "/sessions/"+session+"/conversation", nil, &conv)
		if err == nil {
			if replied(conv.Messages, messageID) {
				return nil
			}
			switch conv.Session.Status {
			case api.SessionStatusFailed, api.SessionStatusCancelled, api.SessionStatusCompleted, api.SessionStatusSuperseded:
				return fmt.Errorf("session ended %s without a reply", conv.Session.Status)
			}
			if err := r.deliveryEnded(ctx, session, sentID(conv.Messages, messageID)); err != nil {
				return err
			}
			lastErr = fmt.Errorf("no reply yet (session %s)", conv.Session.Status)
		} else {
			lastErr = err
		}
		t := time.NewTimer(r.o.Poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("no reply within %s: %w", r.o.Deadline, lastErr)
		case <-t.C:
		}
	}
}

// deliveryEnded reports a canary message whose delivery already ended without a turn: kagent
// refused it, or it was cancelled. POST /message answers 200 once the message is recorded, so
// without this a refusal looks like a session that is merely slow to answer. A failed read of
// the delivery ledger proves nothing and is ignored; the reply wait goes on.
func (r runner) deliveryEnded(ctx context.Context, session, messageID string) error {
	if messageID == "" {
		return nil
	}
	var native api.NativeSessionInfo
	if r.call(ctx, http.MethodGet, "/sessions/"+session+"/native", nil, &native) != nil {
		return nil
	}
	for _, d := range native.Deliveries {
		if d.MessageId != messageID || (d.State != "failed" && d.State != "cancelled") {
			continue
		}
		if d.Detail != nil && *d.Detail != "" {
			return fmt.Errorf("delivery %s: %s", d.State, *d.Detail)
		}
		return fmt.Errorf("delivery %s", d.State)
	}
	return nil
}

// sentID is the id of the user message the canary waits on: messageID, or the first user
// message of a new session.
func sentID(msgs []api.Message, messageID string) string {
	if messageID != "" {
		return messageID
	}
	if at := slices.IndexFunc(msgs, func(m api.Message) bool { return m.Role == api.MessageRoleUser }); at >= 0 {
		return msgs[at].Id
	}
	return ""
}

func replied(msgs []api.Message, messageID string) bool {
	at := slices.IndexFunc(msgs, func(m api.Message) bool {
		return m.Role == api.MessageRoleUser && (messageID == "" || m.Id == messageID)
	})
	if at < 0 {
		return false
	}
	return slices.ContainsFunc(msgs[at+1:], func(m api.Message) bool {
		return m.Role == api.MessageRoleAssistant && strings.TrimSpace(m.Content) != ""
	})
}

func (r runner) call(ctx context.Context, method, path string, payload, out any) error {
	ctx, cancel := context.WithTimeout(ctx, r.o.Request)
	defer cancel()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.o.API+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.o.Host != "" {
		req.Host = r.o.Host
	}
	resp, err := r.o.Client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%s %s: timed out after %s", method, route(path), r.o.Request)
		}
		return fmt.Errorf("%s %s: %w", method, route(path), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: status %d", method, route(path), resp.StatusCode)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode: %w", method, route(path), err)
	}
	return nil
}

// route drops ids from a path for error text: /sessions/{id}/message.
func route(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) > 2 && parts[1] == "sessions" {
		parts[2] = "{id}"
	}
	return strings.Join(parts, "/")
}
