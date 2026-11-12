// Package canary runs one cheap synthetic turn through a running Mainloop and reports which step
// failed or was slow (docs/specs/canary.md). It is the one-shot body of `mainloop canary`, meant
// for a CronJob: it keeps no state of its own; a session a crashed run left behind is found by its
// title and retired by the next run.
//
// The turn is the cheapest one the product can run: a one-word prompt on the smallest model, set
// at create so even the first turn uses it, in a fresh standalone session that the run cancels and
// archives when it ends. A standalone session has no coordination grant, so it never reports to the
// main thread, and it starts no task.
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
	StepReply   = "await-reply"
	StepCleanup = "cleanup"
)

type runner struct {
	o Options
}

// Run does one canary turn in a session of its own, and always cancels and archives that session
// afterwards, so the owner's sessions list and inbox never keep it.
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
	if err := r.call(ctx, http.MethodGet, "/sessions", nil, &sessions); err != nil {
		return fail(StepList, err)
	}
	// Sessions an earlier run could not clean up (it crashed or timed out) go first.
	var stale []string
	for _, s := range sessions {
		if strings.HasPrefix(s.Title, TitlePrefix) && s.ArchivedAt == nil {
			stale = append(stale, s.Id)
		}
	}

	start := o.Now()
	body := map[string]any{
		"title":       TitlePrefix + start.UTC().Format("2006-01-02 15:04"),
		"description": "Synthetic turn that checks Mainloop end to end; archived when it ends (docs/specs/canary.md).",
		"prompt":      Prompt,
	}
	if o.Model != "" {
		body["model"] = o.Model
	}
	var created api.Session
	if err := r.call(ctx, http.MethodPost, "/sessions", body, &created); err != nil {
		res = fail(StepCreate, err)
	} else {
		res.Session = created.Id
		stale = append(stale, created.Id)
		if err := r.awaitReply(ctx, created.Id); err != nil {
			res = fail(StepReply, err)
		}
		res.Elapsed = o.Now().Sub(start)
		res.Slow = res.Err == nil && res.Elapsed > o.Slow
	}

	// Cleanup runs even when the run's ctx has ended.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*o.Request)
	defer cancel()
	for _, id := range stale {
		if err := r.retire(cctx, id); err != nil && res.Err == nil {
			res = fail(StepCleanup, err)
		}
	}
	return res
}

// retire stops the session's turn and archives it. A session that already ended answers the
// cancel with 409; the archive is what matters.
func (r runner) retire(ctx context.Context, id string) error {
	if err := r.call(ctx, http.MethodPost, "/sessions/"+id+"/cancel", map[string]any{}, nil); err != nil &&
		!strings.HasSuffix(err.Error(), "status 409") {
		return err
	}
	return r.call(ctx, http.MethodPost, "/sessions/"+id+"/archive", map[string]any{}, nil)
}

// awaitReply waits for an assistant message after the session's first user message.
func (r runner) awaitReply(ctx context.Context, session string) error {
	ctx, cancel := context.WithTimeout(ctx, r.o.Deadline)
	defer cancel()
	var lastErr error
	for {
		var conv api.SessionConversationResponse
		err := r.call(ctx, http.MethodGet, "/sessions/"+session+"/conversation", nil, &conv)
		if err == nil {
			if replied(conv.Messages) {
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

func replied(msgs []api.Message) bool {
	at := slices.IndexFunc(msgs, func(m api.Message) bool { return m.Role == api.MessageRoleUser })
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
