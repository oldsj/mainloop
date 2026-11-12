// Package canary runs one cheap synthetic turn through a running Mainloop and reports which step
// failed or was slow (docs/specs/canary.md). It is the one-shot body of `mainloop canary`, meant
// for a CronJob: it keeps no state of its own, and everything it needs to find again is in
// Mainloop (a session titled with today's date).
//
// The turn is the cheapest one the product can run: a one-word prompt on the smallest model, in a
// session of its own that is replaced every day so its context stays small. It never touches the
// main thread or tasks, so it starts no work there.
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
	Slow    bool
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
		return fmt.Sprintf("slow %s reply in %s session=%s", r.Target, r.Elapsed.Round(time.Second), r.Session)
	default:
		return fmt.Sprintf("ok %s reply in %s session=%s", r.Target, r.Elapsed.Round(100*time.Millisecond), r.Session)
	}
}

// The steps, in order.
const (
	StepList    = "list-sessions"
	StepCreate  = "create-session"
	StepModel   = "set-model"
	StepSend    = "send-message"
	StepReply   = "await-reply"
	StepArchive = "archive-old"
)

type runner struct {
	o Options
}

// Run does one canary turn.
func Run(ctx context.Context, o Options) Result {
	if o.Client == nil {
		o.Client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	r := runner{o: o}
	res := Result{Target: o.Name}
	fail := func(step string, err error) Result {
		res.Step, res.Err = step, err
		return res
	}

	var sessions []api.Session
	if err := r.call(ctx, http.MethodGet, "/sessions", nil, &sessions); err != nil {
		return fail(StepList, err)
	}
	title := TitlePrefix + o.Now().UTC().Format("2006-01-02")
	var current *api.Session
	var old []string
	for i, s := range sessions {
		if !strings.HasPrefix(s.Title, TitlePrefix) || s.ArchivedAt != nil {
			continue
		}
		if s.Title == title && current == nil {
			current = &sessions[i]
		} else {
			old = append(old, s.Id)
		}
	}

	start := o.Now()
	var messageID string
	if current == nil {
		// The create records the prompt as the session's first turn. The model is set right after:
		// the native session takes seconds to start, so the override normally lands before the
		// first turn is sent; if it does not, that one turn runs on the agent's default model.
		var created api.Session
		body := map[string]any{"title": title, "description": "Synthetic turn that checks Mainloop end to end (docs/specs/canary.md).", "prompt": Prompt}
		if err := r.call(ctx, http.MethodPost, "/sessions", body, &created); err != nil {
			return fail(StepCreate, err)
		}
		res.Session = created.Id
		if o.Model != "" {
			if err := r.call(ctx, http.MethodPost, "/sessions/"+created.Id+"/model", map[string]any{"model": o.Model}, nil); err != nil {
				return fail(StepModel, err)
			}
		}
	} else {
		res.Session = current.Id
		messageID = uuid.NewString()
		var sent api.SessionMessageResponse
		if err := r.call(ctx, http.MethodPost, "/sessions/"+current.Id+"/message",
			map[string]any{"message": Prompt, "message_id": messageID}, &sent); err != nil {
			return fail(StepSend, err)
		}
		if sent.Status != api.SessionMessageResponseStatusOk {
			// Queued or steered behind an open turn: the previous canary turn never ended.
			return fail(StepSend, fmt.Errorf("message %s: an earlier turn is still open", sent.Status))
		}
	}

	if err := r.awaitReply(ctx, res.Session, messageID); err != nil {
		res.Elapsed = o.Now().Sub(start)
		return fail(StepReply, err)
	}
	res.Elapsed = o.Now().Sub(start)
	res.Slow = res.Elapsed > o.Slow

	// Yesterday's sessions are archived once today's has answered. A failure here is reported but
	// is not an outage.
	for _, id := range old {
		if err := r.call(ctx, http.MethodPost, "/sessions/"+id+"/archive", map[string]any{}, nil); err != nil {
			return fail(StepArchive, err)
		}
	}
	return res
}

// awaitReply waits for an assistant message after the user message messageID (or, for a new
// session, after its first user message).
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

func replied(msgs []api.Message, messageID string) bool {
	at := -1
	for i, m := range msgs {
		if m.Role != api.MessageRoleUser {
			continue
		}
		if messageID == "" || m.Id == messageID {
			at = i
			break
		}
	}
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
