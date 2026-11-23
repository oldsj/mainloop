package privileged_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/attention"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/privileged"
	"github.com/oldsj/mainloop/server/internal/privileged/privilegedtest"
	"github.com/oldsj/mainloop/server/internal/sessions"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

const (
	owner = "owner-1"
	repo  = "octo-org/widgets"
	head  = "0123456789abcdef0123456789abcdef01234567"
	moved = "89abcdef0123456789abcdef0123456789abcdef"
)

var mainSession = privileged.Requester{Role: "main", SessionID: "main-session"}

type sent struct{ session, id, text string }

type fakeSender struct {
	mu   sync.Mutex
	err  error
	msgs []sent
}

func (f *fakeSender) SendToWith(_ context.Context, _ sessions.Source, sessionID, messageID, text string, _ sessions.SendOptions) (sessions.Sent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return sessions.Sent{}, f.err
	}
	f.msgs = append(f.msgs, sent{sessionID, messageID, text})
	return sessions.Sent{}, nil
}

type env struct {
	db     *testdb.DB
	gh     *privilegedtest.GitHub
	svc    *privileged.Service
	runner *privileged.Runner
	sender *fakeSender
	inbox  *attention.Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d := testdb.New(t)
	e := &env{db: d, gh: privilegedtest.New(), sender: &fakeSender{}}
	e.svc = &privileged.Service{Pool: d.Pool, OwnerID: owner, GitHub: e.gh, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	e.runner = &privileged.Runner{Service: e.svc, Sender: e.sender}
	e.inbox = &attention.Service{Pool: d.Pool, OwnerID: owner}
	return e
}

func (e *env) run(t *testing.T) {
	t.Helper()
	if err := e.runner.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (e *env) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.db.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func (e *env) get(t *testing.T, id string) privileged.Action {
	t.Helper()
	a, _, err := e.svc.Get(context.Background(), mainSession.SessionID, id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (e *env) openItems(t *testing.T) []api.AttentionItem {
	t.Helper()
	snap, err := e.inbox.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return snap.Items
}

func (e *env) answer(t *testing.T, a privileged.Action, response string, meta map[string]any) {
	t.Helper()
	body := api.QueueItemResponse{Response: response}
	if meta != nil {
		body.Metadata = &meta
	}
	if err := e.inbox.Respond(context.Background(), a.QueueItemID, body); err != nil {
		t.Fatal(err)
	}
}

func mergeArgs(sha string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"repo": repo, "pr": 7, "expected_head_sha": sha})
	return b
}

func (e *env) requestMerge(t *testing.T, requestID string) privileged.Action {
	t.Helper()
	a, err := e.svc.Request(context.Background(), mainSession, privileged.TypePRMerge, mergeArgs(head), "CI is green and the owner asked to ship it.", requestID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Given an open pull request at the expected head, when main requests a merge, then preflight
// freezes its facts under a digest and one approval card shows the exact target, the facts and
// the agent's reason labelled as its claim.
func TestRequestOpensOneCardBoundToADigest(t *testing.T) {
	e := newEnv(t)
	e.gh.Add(repo, 7, head)
	e.gh.Runs = []github.CheckRun{{Name: "build", Status: "completed", Conclusion: "success"}, {Name: "lint", Status: "completed", Conclusion: "failure"}}

	a := e.requestMerge(t, "req-1")
	if a.State != privileged.StatePendingApproval || len(a.Digest) != 64 || a.Replayed {
		t.Fatalf("action = %+v", a)
	}
	items := e.openItems(t)
	if len(items) != 1 || items[0].QueueItem == nil || items[0].QueueItem.Id != a.QueueItemID || items[0].Kind != api.AttentionItemKindApproval {
		t.Fatalf("items = %+v", items)
	}
	q := items[0].QueueItem
	if q.Title != "Merge octo-org/widgets#7" || q.Context["privileged_action_digest"] != a.Digest ||
		q.Context["pr_url"] != "https://github.com/octo-org/widgets/pull/7" || strings.Join(*q.Options, ",") != "Approve,Deny" {
		t.Fatalf("card = %+v", q)
	}
	for _, want := range []string{
		"Main asks: Merge octo-org/widgets#7.",
		"Squash-merge head 0123456789ab (feature-7) into main",
		"Checks on the head: 1 passed, 1 failed, 0 pending. Failing: lint.",
		"Risk: checks failing.",
		"Agent's reason (its claim, not checked by Mainloop): CI is green and the owner asked to ship it.",
		"digest " + a.Digest[:16],
	} {
		if !strings.Contains(q.Content, want) {
			t.Errorf("card lacks %q:\n%s", want, q.Content)
		}
	}

	again := e.requestMerge(t, "req-1")
	if again.ID != a.ID || !again.Replayed {
		t.Fatalf("replay = %+v", again)
	}
	sameArgs := e.requestMerge(t, "req-2")
	if sameArgs.ID != a.ID || !sameArgs.Replayed {
		t.Fatalf("same arguments under a new request id = %+v", sameArgs)
	}
	if _, err := e.svc.Request(context.Background(), mainSession, privileged.TypePRMerge, mergeArgs(moved), "x", "req-1"); !errors.Is(err, privileged.ErrConflict) {
		t.Fatalf("changed replay err = %v", err)
	}
	if n := len(e.openItems(t)); n != 1 {
		t.Errorf("cards = %d", n)
	}
}

// Given arguments outside the contract or a target the action cannot apply to, when an agent
// requests, then nothing goes to the owner.
func TestRequestRefusals(t *testing.T) {
	e := newEnv(t)
	e.gh.Add(repo, 7, head)
	ctx := context.Background()
	for name, raw := range map[string]string{
		"short sha":     `{"repo":"octo-org/widgets","pr":7,"expected_head_sha":"0123"}`,
		"unknown field": `{"repo":"octo-org/widgets","pr":7,"expected_head_sha":"` + head + `","method":"rebase"}`,
		"bad repo":      `{"repo":"widgets","pr":7,"expected_head_sha":"` + head + `"}`,
	} {
		if _, err := e.svc.Request(ctx, mainSession, privileged.TypePRMerge, json.RawMessage(raw), "r", "req-"+name); !errors.Is(err, privileged.ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := e.svc.Request(ctx, mainSession, "command.kubectl", json.RawMessage(`{}`), "r", "req-x"); !errors.Is(err, privileged.ErrUnsupported) {
		t.Errorf("unsupported type err = %v", err)
	}
	secret := `{"repo":"octo-org/widgets","prs":[7],"comment":"token=ghp_abcdefghijklmnop1234"}`
	if _, err := e.svc.Request(ctx, mainSession, privileged.TypePRClose, json.RawMessage(secret), "r", "req-s"); !errors.Is(err, privileged.ErrInvalid) {
		t.Errorf("secret comment err = %v", err)
	}

	a, err := e.svc.Request(ctx, mainSession, privileged.TypePRMerge, mergeArgs(moved), "r", "req-moved")
	if err != nil || a.State != privileged.StatePreflightFailed || a.LastError == nil || !strings.Contains(*a.LastError, "request again with the current head") {
		t.Fatalf("moved head = %+v, %v", a, err)
	}
	e.gh.Set(repo, 7, func(p *github.PullRequestInfo) { p.Draft = true })
	if a, _ := e.svc.Request(ctx, mainSession, privileged.TypePRMerge, mergeArgs(head), "r", "req-draft"); a.State != privileged.StatePreflightFailed {
		t.Fatalf("draft = %+v", a)
	}
	if n := len(e.openItems(t)); n != 0 {
		t.Errorf("a refused request opened %d cards", n)
	}
}

// Given three undecided requests of one session, when it asks a fourth time, then it is refused
// until one is cancelled; cancelling closes the card.
func TestPendingLimitAndCancel(t *testing.T) {
	e := newEnv(t)
	var first privileged.Action
	for i, n := range []int{1, 2, 3} {
		e.gh.Add(repo, n, head)
		b, _ := json.Marshal(map[string]any{"repo": repo, "pr": n, "expected_head_sha": head})
		a, err := e.svc.Request(context.Background(), mainSession, privileged.TypePRMerge, b, "r", "req-"+string(rune('a'+i)))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = a
		}
	}
	e.gh.Add(repo, 4, head)
	b, _ := json.Marshal(map[string]any{"repo": repo, "pr": 4, "expected_head_sha": head})
	if _, err := e.svc.Request(context.Background(), mainSession, privileged.TypePRMerge, b, "r", "req-d"); !errors.Is(err, privileged.ErrTooManyPending) {
		t.Fatalf("fourth err = %v", err)
	}
	if _, err := e.svc.Cancel(context.Background(), "other-session", first.ID); !errors.Is(err, privileged.ErrNotFound) {
		t.Fatalf("another session's cancel err = %v", err)
	}
	c, err := e.svc.Cancel(context.Background(), mainSession.SessionID, first.ID)
	if err != nil || c.State != privileged.StateCancelled {
		t.Fatalf("cancel = %+v, %v", c, err)
	}
	if n := len(e.openItems(t)); n != 2 {
		t.Errorf("open cards after cancel = %d", n)
	}
	if _, err := e.svc.Request(context.Background(), mainSession, privileged.TypePRMerge, b, "r", "req-d"); err != nil {
		t.Fatalf("after cancel err = %v", err)
	}
}

// Given an approval carrying the card's digest, when the runner runs, then it merges exactly that
// head once, comments on the pull request naming the action and sends the session one message.
func TestApprovedMergeRunsOnceAndReports(t *testing.T) {
	e := newEnv(t)
	e.gh.Add(repo, 7, head)
	a := e.requestMerge(t, "req-1")

	e.answer(t, a, privileged.AnswerApprove, map[string]any{"reviewed_context": a.Digest})
	e.run(t)
	e.run(t)

	got := e.get(t, a.ID)
	if got.State != privileged.StateSucceeded || !strings.Contains(string(got.Result), "f00dfeed") {
		t.Fatalf("action = %+v (%s)", got, got.Result)
	}
	if len(e.gh.Merges) != 1 || len(e.gh.Comments) != 1 || !strings.Contains(e.gh.Comments[0], a.ID) {
		t.Fatalf("merges %v comments %v", e.gh.Merges, e.gh.Comments)
	}
	if len(e.sender.msgs) != 1 || e.sender.msgs[0].id != a.ID || e.sender.msgs[0].session != mainSession.SessionID ||
		!strings.Contains(e.sender.msgs[0].text, "ended: succeeded") {
		t.Fatalf("messages = %+v", e.sender.msgs)
	}
	if n := len(e.openItems(t)); n != 0 {
		t.Errorf("open cards = %d", n)
	}
	_, evs, _ := e.svc.Get(context.Background(), mainSession.SessionID, a.ID)
	var steps []string
	for _, ev := range evs {
		steps = append(steps, ev.Step)
	}
	if strings.Join(steps, ",") != "requested,preflighted,decided,claimed,executed,delivered" {
		t.Errorf("events = %v", steps)
	}
}

// Given an Approve without the card's digest, or any other answer, then it is a denial and the
// note goes to the session; nothing runs.
func TestApproveWithoutDigestAndNotesDeny(t *testing.T) {
	e := newEnv(t)
	e.gh.Add(repo, 7, head)
	e.gh.Add(repo, 8, head)
	a := e.requestMerge(t, "req-1")
	b, err := e.svc.Request(context.Background(), mainSession, privileged.TypePRMerge,
		json.RawMessage(`{"repo":"octo-org/widgets","pr":8,"expected_head_sha":"`+head+`"}`), "r", "req-2")
	if err != nil {
		t.Fatal(err)
	}
	e.answer(t, a, privileged.AnswerApprove, map[string]any{"reviewed_context": strings.Repeat("0", 64)})
	e.answer(t, b, "Not yet, wait for the release.", nil)
	e.run(t)

	if got := e.get(t, a.ID); got.State != privileged.StateDenied || got.DecisionNote == nil || !strings.Contains(*got.DecisionNote, "digest") {
		t.Fatalf("wrong digest = %+v", got)
	}
	if got := e.get(t, b.ID); got.State != privileged.StateDenied || got.DecisionNote == nil || *got.DecisionNote != "Not yet, wait for the release." {
		t.Fatalf("note = %+v", got)
	}
	if len(e.gh.Merges) != 0 {
		t.Fatalf("merged %v", e.gh.Merges)
	}
	if len(e.sender.msgs) != 2 || !strings.Contains(e.sender.msgs[0].text+e.sender.msgs[1].text, "The owner's note: Not yet") {
		t.Fatalf("messages = %+v", e.sender.msgs)
	}
}

// Given an approved merge whose head moved meanwhile, then nothing merges and the action is stale.
func TestHeadMovedAfterApprovalIsStale(t *testing.T) {
	e := newEnv(t)
	e.gh.Add(repo, 7, head)
	a := e.requestMerge(t, "req-1")
	e.answer(t, a, privileged.AnswerApprove, map[string]any{"reviewed_context": a.Digest})
	e.gh.Set(repo, 7, func(p *github.PullRequestInfo) { p.Head.SHA = moved })
	e.run(t)
	if got := e.get(t, a.ID); got.State != privileged.StateStale || len(e.gh.Merges) != 0 {
		t.Fatalf("action = %+v, merges %v", got, e.gh.Merges)
	}
}

// Given a merge whose answer was lost after GitHub applied it, when the runner tries again, then
// it observes the merge and succeeds without merging twice.
func TestLostAnswerIsObservedNotRepeated(t *testing.T) {
	e := newEnv(t)
	e.gh.Add(repo, 7, head)
	a := e.requestMerge(t, "req-1")
	e.answer(t, a, privileged.AnswerApprove, map[string]any{"reviewed_context": a.Digest})
	e.gh.MergeErr, e.gh.Applied = errors.New("connection reset"), true
	e.run(t)
	if got := e.get(t, a.ID); got.State != privileged.StateExecuting {
		t.Fatalf("after lost answer = %+v", got)
	}
	e.exec(t, `UPDATE privileged_actions SET next_attempt_at = now() WHERE id = $1`, a.ID)
	e.run(t)
	got := e.get(t, a.ID)
	if got.State != privileged.StateSucceeded || !strings.Contains(nospace(got.Result), `"observed":true`) || len(e.gh.Merges) != 1 {
		t.Fatalf("after observe = %+v (%s), merges %v", got, got.Result, e.gh.Merges)
	}
}

// Given an approved action whose stored binding changed after approval, then nothing runs.
func TestChangedBindingRunsNothing(t *testing.T) {
	e := newEnv(t)
	e.gh.Add(repo, 7, head)
	a := e.requestMerge(t, "req-1")
	e.answer(t, a, privileged.AnswerApprove, map[string]any{"reviewed_context": a.Digest})
	e.run(t) // records the decision and runs it; reset to approved to model a tampered row
	if len(e.gh.Merges) != 1 {
		t.Fatalf("merges %v", e.gh.Merges)
	}

	e.gh.Add(repo, 9, head)
	b, err := e.svc.Request(context.Background(), mainSession, privileged.TypePRMerge,
		json.RawMessage(`{"repo":"octo-org/widgets","pr":9,"expected_head_sha":"`+head+`"}`), "r", "req-2")
	if err != nil {
		t.Fatal(err)
	}
	e.answer(t, b, privileged.AnswerApprove, map[string]any{"reviewed_context": b.Digest})
	e.exec(t, `UPDATE privileged_actions SET expires_at = expires_at + interval '1 day' WHERE id = $1`, b.ID)
	e.run(t)
	if got := e.get(t, b.ID); got.State != privileged.StateFailed || got.LastError == nil || !strings.Contains(*got.LastError, "no longer matches") {
		t.Fatalf("tampered = %+v", got)
	}
	if len(e.gh.Merges) != 1 {
		t.Fatalf("merges %v", e.gh.Merges)
	}
	if _, err := e.db.Pool.Exec(context.Background(), `UPDATE privileged_actions SET args = '{}' WHERE id = $1`, b.ID); err == nil {
		t.Fatal("the arguments could be changed after the request")
	}
}

// Given an undecided action past its expiry, then it expires, its card closes and the session
// hears once.
func TestExpiry(t *testing.T) {
	e := newEnv(t)
	e.gh.Add(repo, 7, head)
	a := e.requestMerge(t, "req-1")
	e.exec(t, `UPDATE privileged_actions SET expires_at = now() - interval '1 second', digest = digest WHERE id = $1`, a.ID)
	e.run(t)
	if got := e.get(t, a.ID); got.State != privileged.StateExpired {
		t.Fatalf("action = %+v", got)
	}
	if n := len(e.openItems(t)); n != 0 {
		t.Errorf("open cards = %d", n)
	}
	if len(e.sender.msgs) != 1 || !strings.Contains(e.sender.msgs[0].text, "expired") {
		t.Fatalf("messages = %+v", e.sender.msgs)
	}
}

// Given a close of two pull requests where one head moved, then the other closes, the moved one
// stays open and the action reports which.
func TestCloseLeavesAMovedHeadOpen(t *testing.T) {
	e := newEnv(t)
	e.gh.Add(repo, 3, head)
	e.gh.Add(repo, 4, head)
	a, err := e.svc.Request(context.Background(), mainSession, privileged.TypePRClose,
		json.RawMessage(`{"repo":"octo-org/widgets","prs":[4,3,4],"comment":"Superseded by #5."}`), "Superseded.", "req-1")
	if err != nil || a.State != privileged.StatePendingApproval {
		t.Fatalf("request = %+v, %v", a, err)
	}
	if !strings.Contains(nospace(a.Args), `"prs":[3,4]`) {
		t.Errorf("args not canonical: %s", a.Args)
	}
	e.answer(t, a, privileged.AnswerApprove, map[string]any{"reviewed_context": a.Digest})
	e.gh.Set(repo, 4, func(p *github.PullRequestInfo) { p.Head.SHA = moved })
	e.run(t)
	got := e.get(t, a.ID)
	if got.State != privileged.StateFailed || !strings.Contains(nospace(got.Result), `{"number":3,"result":"closed"}`) ||
		!strings.Contains(nospace(got.Result), `"number":4,"result":"stale"`) {
		t.Fatalf("action = %+v (%s)", got, got.Result)
	}
	if len(e.gh.Closes) != 1 || !strings.HasPrefix(e.gh.Comments[0], "octo-org/widgets#3: Superseded by #5.") {
		t.Fatalf("closes %v comments %v", e.gh.Closes, e.gh.Comments)
	}
}

// Given a session that is gone, then the outcome is recorded undeliverable rather than retried.
func TestUndeliverable(t *testing.T) {
	e := newEnv(t)
	e.gh.Add(repo, 7, head)
	a := e.requestMerge(t, "req-1")
	e.sender.err = sessions.ErrNotFound
	e.answer(t, a, privileged.AnswerDeny, nil)
	e.run(t)
	var st string
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT delivery_state FROM privileged_actions WHERE id = $1`, a.ID).Scan(&st); err != nil || st != "undeliverable" {
		t.Fatalf("delivery = %q, %v", st, err)
	}
}

func nospace(b json.RawMessage) string { return strings.ReplaceAll(string(b), " ", "") }
