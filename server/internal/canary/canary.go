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
	// Restarted is the refusal that made the run restart the canary session and ping it again;
	// empty when it did not. A run that restarted is reported once even when the second ping
	// was answered.
	Restarted string
}

// OK reports whether the run passed in time without restarting the session.
func (r Result) OK() bool { return r.Err == nil && !r.Slow && r.Restarted == "" }

// String is the one-line report.
func (r Result) String() string {
	switch {
	case r.Err != nil:
		return fmt.Sprintf("fail %s step=%s session=%s: %v", r.Target, r.Step, r.Session, r.Err)
	case r.Restarted != "":
		return fmt.Sprintf("restarted %s session=%s after kagent refused the ping (%s); reply in %s",
			r.Target, r.Session, r.Restarted, r.Elapsed.Round(100*time.Millisecond))
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
		id, err := r.ping(ctx, current.Id)
		if err != nil {
			return fail(StepSend, err)
		}
		messageID = id
	}

	err := r.awaitReply(ctx, res.Session, messageID)
	var ended *deliveryEnded
	if errors.As(err, &ended) && ended.refused() && ended.restartable {
		// kagent refuses the session's input, so no ping can get through until its native
		// Session is replaced: restart it, as the owner would from the chat, and ping again.
		res.Restarted = ended.detail
		if rerr := r.restart(ctx, res.Session); rerr != nil {
			res.Elapsed = o.Now().Sub(start)
			return fail(StepReply, fmt.Errorf("%w; restarting the session failed: %w", err, rerr))
		}
		id, perr := r.ping(ctx, res.Session)
		if perr != nil {
			res.Elapsed = o.Now().Sub(start)
			return fail(StepSend, fmt.Errorf("after restarting the session: %w", perr))
		}
		if err = r.awaitReply(ctx, res.Session, id); err != nil {
			err = fmt.Errorf("after restarting the session: %w", err)
		}
	}
	res.Elapsed = o.Now().Sub(start)
	if err != nil {
		r.stop(ctx, res.Session)
		return fail(StepReply, err)
	}
	res.Slow = res.Elapsed > o.Slow
	return res
}

// ping sends the canary prompt to an existing session and returns its message id.
func (r runner) ping(ctx context.Context, session string) (string, error) {
	id := uuid.NewString()
	var sent api.SessionMessageResponse
	if err := r.call(ctx, http.MethodPost, "/sessions/"+session+"/message",
		map[string]any{"message": Prompt, "message_id": id}, &sent); err != nil {
		return "", err
	}
	if sent.Status != api.SessionMessageResponseStatusOk {
		// Queued or steered behind an open turn: an earlier canary turn never ended.
		r.stop(ctx, session)
		return "", fmt.Errorf("message %s: an earlier turn is still open; stopped it", sent.Status)
	}
	return id, nil
}

// restart replaces the session's native kagent Session (POST /sessions/{id}/restart). The next
// message creates a new one.
func (r runner) restart(ctx context.Context, session string) error {
	return r.call(ctx, http.MethodPost, "/sessions/"+session+"/restart",
		map[string]any{"request_id": uuid.NewString()}, nil)
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
			if ended := r.deliveryEnded(ctx, session, sentID(conv.Messages, messageID)); ended != nil {
				return ended
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

// deliveryEnded is a canary message whose delivery already ended without a turn: kagent
// refused it, or it was cancelled. POST /message answers 200 once the message is recorded, so
// without this a refusal looks like a session that is merely slow to answer.
type deliveryEnded struct {
	state, detail string
	// restartable is the session's POST /sessions/{id}/restart applies.
	restartable bool
}

func (e *deliveryEnded) Error() string {
	if e.detail == "" {
		return "delivery " + e.state
	}
	return "delivery " + e.state + ": " + e.detail
}

// refused reports a delivery kagent answered with an A2A error, such as -32004 for a session that
// accepts no input. A restart replaces that native Session; it cannot help a kagent that did not
// answer at all.
func (e *deliveryEnded) refused() bool {
	return e.state == "failed" &&
		(strings.HasPrefix(e.detail, "not sent: A2A error ") || strings.HasPrefix(e.detail, "send rejected: A2A error "))
}

// deliveryEnded reads the delivery ledger for messageID. A failed read proves nothing and is
// nil; the reply wait goes on.
func (r runner) deliveryEnded(ctx context.Context, session, messageID string) *deliveryEnded {
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
		e := &deliveryEnded{state: d.State, restartable: native.Restartable != nil && *native.Restartable}
		if d.Detail != nil {
			e.detail = *d.Detail
		}
		return e
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
