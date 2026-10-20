package merge_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/merge"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

const (
	owner = "owner-1"
	tip   = "1111111111111111111111111111111111111111"
	moved = "2222222222222222222222222222222222222222"
)

var mergePath = "/repos/o/n/pulls/1/merge"

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

type env struct {
	t    *testing.T
	rt   *runtimeFake
	pool *pgxpool.Pool
	fake *githubtest.Server
	svc  *merge.Service
	now  atomic.Int64 // offset added to the clock, in seconds
}

var claudeConfig = config.MergeConfiguration{
	TemplateName: "t", Provider: config.NativeClaude, CompiledAlias: "mainloop_merge",
	Endpoint: "http://x/mcp/merge-approval", Tool: config.MergeToolName, RequireApproval: true, Operation: config.MergeOperation,
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d := testdb.New(t)
	fake := githubtest.New(t)
	fake.AddRepo(githubtest.Repository{
		Owner: "o", Name: "n", ID: 7, Branches: map[string]string{"feature": tip},
		PullRequests: []githubtest.PullRequest{{
			Number: 1, Title: "Add a thing", Body: "Because.", HeadRef: "feature", HeadSHA: tip,
			Files: []githubtest.File{{Filename: "server/a.go", Additions: 3, Deletions: 1}},
		}},
		Suites: map[string][]githubtest.CheckSuite{tip: {{ID: 1, AppID: 9, Conclusion: "success"}}},
		Runs:   map[string][]githubtest.CheckRun{tip: {{ID: 1, AppID: 9, Name: "ci", Conclusion: "success"}}},
	})
	app, err := github.NewApp(github.Options{AppID: fmt.Sprint(githubtest.AppID), PrivateKey: githubtest.PrivateKey(), BaseURL: fake.URL})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, pool: d.Pool, fake: fake, rt: newRuntimeFake()}
	e.svc = &merge.Service{
		Pool: d.Pool, GitHub: app, Log: quiet(), Configs: []config.MergeConfiguration{claudeConfig}, Runtime: e.rt,
		Settle: 5 * time.Second, Deadline: 10 * time.Minute, Poll: 20 * time.Millisecond, Window: time.Minute,
		Recheck: 10 * time.Millisecond, Ceiling: 20 * time.Millisecond, Lease: time.Minute, SweepEvery: 20 * time.Millisecond,
		Now: func() time.Time { return time.Now().Add(time.Duration(e.now.Load()) * time.Second) },
	}
	e.exec(`INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ('p1', $1, 'o', 'n', 'o/n', 'https://x')`, owner)
	e.exec(`INSERT INTO sessions (id, user_id, project_id, title, description, prompt) VALUES ('s1', $1, 'p1', 't', 'd', 'p')`, owner)
	e.exec(`INSERT INTO agent_bindings (session_id, kind, role, kagent_session_id) VALUES ('s1', 'claude', 'agent', 'k1')`)
	return e
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

// run starts the merge loop and the completion sweep until the test ends.
func (e *env) run() {
	e.t.Helper()
	r := reconcile.NewRunner(e.pool, quiet(), nil)
	l := e.svc.Loop()
	l.Interval = 10 * time.Millisecond
	r.Add(l)
	r.AddSweep(e.svc.CompletionSweep())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	e.t.Cleanup(func() { cancel(); <-done })
}

func (e *env) input(req string) merge.PrepareInput {
	return merge.PrepareInput{OwnerID: owner, ProjectID: "p1", SessionID: "s1", PRNumber: 1, ExpectedSHA: tip, RequestID: req}
}

func (e *env) puts() int { return e.fake.Count("PUT", mergePath) }

func (e *env) state(proposal string) dbstate.MergeState {
	e.t.Helper()
	var s dbstate.MergeState
	err := e.pool.QueryRow(context.Background(),
		`SELECT c.state FROM merge_candidates c JOIN merge_proposals p ON p.candidate_id = c.id WHERE p.id = $1`, proposal).Scan(&s)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) detail(proposal string) string {
	e.t.Helper()
	var d *string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT c.detail FROM merge_candidates c JOIN merge_proposals p ON p.candidate_id = c.id WHERE p.id = $1`, proposal).Scan(&d); err != nil {
		e.t.Fatal(err)
	}
	if d == nil {
		return ""
	}
	return *d
}

func (e *env) pr() githubtest.PullRequest {
	p, _ := e.fake.PullRequestOf("o", "n", 1)
	return p
}

func (e *env) policy(p string) {
	e.exec(`UPDATE projects SET merge_policy = $1 WHERE id = 'p1'`, p)
}

// settled waits for a proposal to reach a final state.
func (e *env) settled(proposal string) dbstate.MergeState {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s := e.state(proposal); merge.Final(s) {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("proposal %s never settled (state %s)", proposal, e.state(proposal))
	return ""
}

func TestAutoMergeSquashesOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	ctx := context.Background()
	v, err := e.svc.Merge(ctx, e.input("r1"))
	if err != nil {
		t.Fatal(err)
	}
	if v.State != dbstate.MergeMerged || v.Status != "merged" || len(v.MergeSHA) != 40 || v.Route != dbstate.RouteAuto {
		t.Fatalf("view = %+v", v)
	}
	if !e.pr().Merged || e.puts() != 1 {
		t.Fatalf("merged = %v, puts = %d", e.pr().Merged, e.puts())
	}
	// The same call again reports the stored result and sends nothing.
	again, err := e.svc.Merge(ctx, e.input("r1"))
	if err != nil || again.State != dbstate.MergeMerged || again.MergeSHA != v.MergeSHA || e.puts() != 1 {
		t.Fatalf("replay = %+v, %v, puts %d", again, err, e.puts())
	}
	// The final outcome is in the ledger once.
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM merge_results WHERE proposal_id = $1 AND state = 'merged'`, v.ProposalID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("results = %d, %v", n, err)
	}
	// A new request for the merged PR is refused, not merged again.
	_, err = e.svc.Merge(ctx, e.input("r2"))
	var ref *merge.Refusal
	if !errors.As(err, &ref) || ref.Code != "already_merged" && ref.Code != "pull_request_not_open" || e.puts() != 1 {
		t.Fatalf("second merge = %v, puts %d", err, e.puts())
	}
}

func TestRequestIDWithOtherArgumentsConflicts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.Prepare(ctx, e.input("r1")); err != nil {
		t.Fatal(err)
	}
	other := e.input("r1")
	other.ExpectedSHA = moved
	if _, err := e.svc.Prepare(ctx, other); !errors.Is(err, merge.ErrConflict) {
		t.Fatalf("err = %v", err)
	}
}

func TestPrepareFreezesFactsAndWritesNoApprovalCard(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	v, err := e.svc.Prepare(context.Background(), e.input("r1"))
	if err != nil {
		t.Fatal(err)
	}
	if v.State != dbstate.MergePrepared || v.Route != dbstate.RouteAuto || v.Summary.Title == nil || *v.Summary.Title != "Add a thing" || v.Digest == "" || v.HeadSHA != tip {
		t.Fatalf("view = %+v", v)
	}
	if e.puts() != 0 {
		t.Fatal("prepare merged")
	}
	// The ledger is append-only.
	for _, q := range []string{
		`UPDATE merge_proposals SET session_id = 'x'`, `DELETE FROM merge_proposals`,
		`UPDATE merge_request_keys SET request_id = 'x'`, `DELETE FROM merge_request_keys`,
	} {
		if _, err := e.pool.Exec(context.Background(), q); err == nil {
			t.Errorf("%s was allowed", q)
		}
	}
}

func TestPrepareRefusals(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	code := func(in merge.PrepareInput) string {
		_, err := e.svc.Prepare(ctx, in)
		var r *merge.Refusal
		if errors.As(err, &r) {
			return r.Code
		}
		if err != nil {
			return err.Error()
		}
		return ""
	}
	in := e.input("a")
	in.ExpectedSHA = moved
	if got := code(in); got != "head_moved" {
		t.Errorf("wrong sha = %q", got)
	}
	in = e.input("b")
	in.PRNumber = 99
	if got := code(in); got != "pull_request_not_found" {
		t.Errorf("unknown pr = %q", got)
	}
	in = e.input("c")
	in.ProjectID = "nope"
	if _, err := e.svc.Prepare(ctx, in); !errors.Is(err, merge.ErrNotFound) {
		t.Errorf("unknown project = %v", err)
	}
	in = e.input("c2")
	in.OwnerID = "someone-else"
	if _, err := e.svc.Prepare(ctx, in); !errors.Is(err, merge.ErrNotFound) {
		t.Errorf("foreign owner = %v", err)
	}
	e.fake.Update("o", "n", func(r *githubtest.Repository) { r.PullRequests[0].Draft = true })
	if got := code(e.input("d")); got != "" {
		// A draft is not refused at prepare time: the proposal records that the evidence is
		// unavailable and takes the approval route, which cannot be executed.
		t.Errorf("draft prepare = %q", got)
	}
	v, err := e.svc.Prepare(ctx, e.input("d"))
	if err != nil || v.Route != dbstate.RouteApproval || len(v.Summary.UnavailableReasons) == 0 {
		t.Errorf("draft view = %+v, %v", v, err)
	}
	e.fake.Update("o", "n", func(r *githubtest.Repository) { r.PullRequests[0].Draft = false })
}

func TestProtectedPathRoutesToApproval(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		r.PullRequests[0].Files = append(r.PullRequests[0].Files, githubtest.File{Filename: "k8s/app.yaml", Additions: 1})
	})
	v, err := e.svc.Merge(context.Background(), e.input("r1"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Route != dbstate.RouteApproval || v.Status != "approval_required" || v.State != dbstate.MergePrepared || e.puts() != 0 {
		t.Fatalf("view = %+v, puts %d", v, e.puts())
	}
	if len(v.Summary.ApprovalReasons) == 0 {
		t.Fatal("no approval reasons")
	}
}

func TestDockerfileOnlyChangeRoutesToApprovalWhenFlagIsOff(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		r.PullRequests[0].Files = append(r.PullRequests[0].Files, githubtest.File{Filename: "server/Dockerfile", Additions: 1})
	})
	v, err := e.svc.Merge(context.Background(), e.input("r1"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Route != dbstate.RouteApproval || v.Status != "approval_required" || v.State != dbstate.MergePrepared || e.puts() != 0 {
		t.Fatalf("view = %+v, puts %d", v, e.puts())
	}
	if len(v.Summary.ApprovalReasons) == 0 {
		t.Fatal("no approval reasons")
	}
}

func TestProtectedAutoMergesAProtectedPathOnGreenWithoutACard(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	e.svc.ProtectedAuto = true
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		r.PullRequests[0].Files = append(r.PullRequests[0].Files, githubtest.File{Filename: "k8s/app.yaml", Additions: 1})
	})
	v, err := e.svc.Merge(context.Background(), e.input("r1"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Route != dbstate.RouteAuto || v.Status != "merged" || v.State != dbstate.MergeMerged || e.puts() != 1 {
		t.Fatalf("view = %+v, puts %d", v, e.puts())
	}
	// The protected match is still recorded in the frozen facts and presentation, for audit, even
	// though it did not force the approval route.
	if len(v.Summary.ProtectedMatches) == 0 || len(v.Summary.ApprovalReasons) == 0 {
		t.Fatalf("protected match not recorded for audit: %+v", v.Summary)
	}
}

func TestProtectedAutoStillHonoursTheProjectsApprovalPolicy(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	e.svc.ProtectedAuto = true
	e.policy("approval")
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		r.PullRequests[0].Files = append(r.PullRequests[0].Files, githubtest.File{Filename: "k8s/app.yaml", Additions: 1})
	})
	v, err := e.svc.Merge(context.Background(), e.input("r1"))
	if err != nil || v.Route != dbstate.RouteApproval || v.Status != "approval_required" || e.puts() != 0 {
		t.Fatalf("view = %+v, %v, puts %d", v, err, e.puts())
	}
}

func TestProtectedAutoRefusesUnavailableEvidenceInsteadOfACard(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	e.svc.ProtectedAuto = true
	e.fake.Update("o", "n", func(r *githubtest.Repository) { r.PullRequests[0].Draft = true })
	// Prepare alone still freezes the facts, on the automatic route, and raises no card.
	v, err := e.svc.Prepare(context.Background(), e.input("r1"))
	if err != nil || v.Route != dbstate.RouteAuto || v.Status != string(dbstate.MergePrepared) || len(v.Summary.UnavailableReasons) == 0 {
		t.Fatalf("prepare view = %+v, %v", v, err)
	}
	// Merge refuses it outright instead of waiting behind an approval card nobody can safely approve.
	got, err := e.svc.Merge(context.Background(), e.input("r2"))
	var ref *merge.Refusal
	if !errors.As(err, &ref) || ref.Code != "evidence_unavailable" || e.puts() != 0 {
		t.Fatalf("merge = %+v, %v, puts %d", got, err, e.puts())
	}
}

func TestTogglingProtectedAutoOffBlocksAnAlreadyQueuedCandidate(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.svc.ProtectedAuto = true
	e.svc.Settle = 10 * time.Millisecond
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		r.PullRequests[0].Files = append(r.PullRequests[0].Files, githubtest.File{Filename: "k8s/app.yaml", Additions: 1})
	})
	// Committed to the auto route and moved to evaluating while the flag is on, with no loop running
	// yet to carry it further.
	v, err := e.svc.Merge(context.Background(), e.input("r1"))
	if err != nil || v.Route != dbstate.RouteAuto || v.State != dbstate.MergeEvaluating {
		t.Fatalf("view = %+v, %v", v, err)
	}

	// A new service instance against the same database, as if the operator had restarted with the
	// flag off, picks the candidate up instead.
	off := *e.svc
	off.ProtectedAuto = false
	r := reconcile.NewRunner(e.pool, quiet(), nil)
	l := off.Loop()
	l.Interval = 10 * time.Millisecond
	r.Add(l)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	if s := e.settled(v.ProposalID); s != dbstate.MergeBlocked {
		t.Fatalf("state = %s", s)
	}
	if got := e.detail(v.ProposalID); got != "protected_auto_disabled" {
		t.Fatalf("detail = %q", got)
	}
	if e.puts() != 0 {
		t.Fatalf("puts = %d", e.puts())
	}
}

func TestApprovalPolicyNeverAutoMerges(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	e.policy("approval")
	v, err := e.svc.Merge(context.Background(), e.input("r1"))
	if err != nil || v.Status != "approval_required" || e.puts() != 0 {
		t.Fatalf("view = %+v, %v, puts %d", v, err, e.puts())
	}
}

func TestWaitsForPendingCIThenMerges(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		r.Runs[tip] = []githubtest.CheckRun{{ID: 1, AppID: 9, Name: "ci", Status: "in_progress"}}
	})
	ctx := context.Background()
	// The tool gives up waiting (Settle) but the intent stays and the loop carries on.
	e.svc.Settle = 200 * time.Millisecond
	v, err := e.svc.Merge(ctx, e.input("r1"))
	if err != nil || v.State != dbstate.MergeEvaluating || e.puts() != 0 {
		t.Fatalf("view = %+v, %v, puts %d", v, err, e.puts())
	}
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		r.Runs[tip] = []githubtest.CheckRun{{ID: 1, AppID: 9, Name: "ci", Conclusion: "success"}}
	})
	if got := e.settled(v.ProposalID); got != dbstate.MergeMerged || e.puts() != 1 {
		t.Fatalf("state = %s, puts %d", got, e.puts())
	}
}

func TestFailedChecksBlock(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		r.Runs[tip] = []githubtest.CheckRun{{ID: 1, AppID: 9, Name: "ci", Conclusion: "failure"}}
	})
	v, err := e.svc.Merge(context.Background(), e.input("r1"))
	if err != nil || v.State != dbstate.MergeBlocked || v.Detail != "checks_failed" || e.puts() != 0 {
		t.Fatalf("view = %+v, %v", v, err)
	}
}

func TestNoCINeverMerges(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	e.fake.Update("o", "n", func(r *githubtest.Repository) { r.Runs = nil })
	e.svc.Settle = 150 * time.Millisecond
	v, err := e.svc.Merge(context.Background(), e.input("r1"))
	if err != nil || v.State != dbstate.MergeEvaluating || e.puts() != 0 {
		t.Fatalf("view = %+v, %v, puts %d", v, err, e.puts())
	}
}

func TestExpiryEndsEvaluation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		r.Runs[tip] = []githubtest.CheckRun{{ID: 1, AppID: 9, Name: "ci", Status: "queued"}}
	})
	e.svc.Settle = 100 * time.Millisecond
	v, err := e.svc.Merge(context.Background(), e.input("r1"))
	if err != nil || v.State != dbstate.MergeEvaluating {
		t.Fatalf("view = %+v, %v", v, err)
	}
	e.now.Store(int64((11 * time.Minute).Seconds()))
	if got := e.settled(v.ProposalID); got != dbstate.MergeExpired || e.puts() != 0 {
		t.Fatalf("state = %s, puts %d", got, e.puts())
	}
}

func TestNewHeadSupersedes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		r.Runs[tip] = []githubtest.CheckRun{{ID: 1, AppID: 9, Name: "ci", Status: "queued"}}
	})
	e.svc.Settle = 100 * time.Millisecond
	v, err := e.svc.Merge(context.Background(), e.input("r1"))
	if err != nil || v.State != dbstate.MergeEvaluating {
		t.Fatalf("view = %+v, %v", v, err)
	}
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		r.PullRequests[0].HeadSHA = moved
		r.Branches["feature"] = moved
	})
	if got := e.settled(v.ProposalID); got != dbstate.MergeSuperseded || e.puts() != 0 {
		t.Fatalf("state = %s, puts %d, detail %q", got, e.puts(), e.detail(v.ProposalID))
	}
}

func TestSecondPreparationSupersedesTheFirst(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	first, err := e.svc.Prepare(ctx, e.input("r1"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.svc.Prepare(ctx, e.input("r2"))
	if err != nil {
		t.Fatal(err)
	}
	if e.state(first.ProposalID) != dbstate.MergeSuperseded || e.state(second.ProposalID) != dbstate.MergePrepared {
		t.Fatalf("states = %s, %s", e.state(first.ProposalID), e.state(second.ProposalID))
	}
}

func TestMergeRefusedByGitHubBlocks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	e.fake.Update("o", "n", func(r *githubtest.Repository) { r.MergeStatus = 403 })
	v, err := e.svc.Merge(context.Background(), e.input("r1"))
	if err != nil || v.State != dbstate.MergeBlocked || v.Detail != "merge_refused_403" {
		t.Fatalf("view = %+v, %v", v, err)
	}
	// The refusal is final: the PUT was sent once and is not retried.
	time.Sleep(100 * time.Millisecond)
	if e.puts() != 1 {
		t.Fatalf("puts = %d", e.puts())
	}
	if strings.Contains(fmt.Sprint(v), githubtest.SecretMarker) {
		t.Fatal("GitHub's refusal text reached the view")
	}
}

func TestRateLimitedMergeIsRetriedNotUncertain(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	e.fake.RateLimit("PUT", mergePath)
	ctx := context.Background()
	e.svc.Settle = 200 * time.Millisecond
	v, err := e.svc.Merge(ctx, e.input("r1"))
	if err != nil || e.state(v.ProposalID) == dbstate.MergeUncertain || e.state(v.ProposalID) == dbstate.MergeMerged {
		t.Fatalf("view = %+v, %v, state %s", v, err, e.state(v.ProposalID))
	}
	e.fake.ClearRateLimit("PUT", mergePath)
	if got := e.settled(v.ProposalID); got != dbstate.MergeMerged {
		t.Fatalf("state = %s", got)
	}
}

func TestLostPUTReplyIsSettledByReadingNotResending(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	// GitHub applies the merge but the reply is lost.
	e.fake.LoseReply("PUT", mergePath, 1)
	v, err := e.svc.Merge(context.Background(), e.input("r1"))
	if err != nil {
		t.Fatal(err)
	}
	if v.State != dbstate.MergeMerged || len(v.MergeSHA) != 40 || e.puts() != 1 {
		t.Fatalf("view = %+v, puts %d", v, e.puts())
	}
}

func TestLostPUTThatNeverRanEndsNotApplied(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	// The reply is lost and nothing was merged (the fake answers 502 without merging).
	e.fake.Fail("PUT", mergePath, 502)
	e.svc.Settle = 300 * time.Millisecond
	v, err := e.svc.Merge(context.Background(), e.input("r1"))
	if err != nil || v.State != dbstate.MergeUncertain {
		t.Fatalf("view = %+v, %v", v, err)
	}
	// While the window is open the row stays uncertain and nothing is sent again.
	time.Sleep(100 * time.Millisecond)
	if e.puts() != 1 || e.state(v.ProposalID) != dbstate.MergeUncertain {
		t.Fatalf("puts = %d, state = %s", e.puts(), e.state(v.ProposalID))
	}
	e.now.Store(int64((2 * time.Minute).Seconds()))
	if got := e.settled(v.ProposalID); got != dbstate.MergeBlocked || e.detail(v.ProposalID) != "merge_not_applied" || e.puts() != 1 {
		t.Fatalf("state = %s (%s), puts %d", got, e.detail(v.ProposalID), e.puts())
	}
}

func TestUncertainResolvesMergedWhenTheMergeLanded(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	v, err := e.svc.Prepare(ctx, e.input("r1"))
	if err != nil {
		t.Fatal(err)
	}
	// A crash after the PUT: the row is merging and GitHub has merged the PR.
	e.exec(`UPDATE merge_candidates SET state = 'merging' WHERE id = (SELECT candidate_id FROM merge_proposals WHERE id = $1)`, v.ProposalID)
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		r.PullRequests[0].Merged, r.PullRequests[0].Closed = true, true
		r.PullRequests[0].MergeCommitSHA = "3333333333333333333333333333333333333333"
	})
	e.run()
	if got := e.settled(v.ProposalID); got != dbstate.MergeMerged || e.puts() != 0 {
		t.Fatalf("state = %s, puts %d", got, e.puts())
	}
}

func TestUncertainBlocksWhenTheHeadMovedOrPRClosed(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*githubtest.Repository){
		"head_moved": func(r *githubtest.Repository) { r.PullRequests[0].HeadSHA = moved },
		"closed_without_merge": func(r *githubtest.Repository) {
			r.PullRequests[0].Closed = true
		},
		"merged_other_head": func(r *githubtest.Repository) {
			r.PullRequests[0].Merged, r.PullRequests[0].Closed = true, true
			r.PullRequests[0].HeadSHA = moved
			r.PullRequests[0].MergeCommitSHA = "3333333333333333333333333333333333333333"
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			v, err := e.svc.Prepare(context.Background(), e.input("r1"))
			if err != nil {
				t.Fatal(err)
			}
			e.exec(`UPDATE merge_candidates SET state = 'uncertain' WHERE id = (SELECT candidate_id FROM merge_proposals WHERE id = $1)`, v.ProposalID)
			e.fake.Update("o", "n", change)
			e.run()
			if got := e.settled(v.ProposalID); got != dbstate.MergeBlocked || e.detail(v.ProposalID) != name || e.puts() != 0 {
				t.Fatalf("state = %s (%s), puts %d", got, e.detail(v.ProposalID), e.puts())
			}
		})
	}
}

// A call that stops at each step of the merge, and a loop that takes over later, sends the PUT at most once.
func TestReplayAfterAStopAtEveryStepSendsOnePUT(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, state string
		// merged is true when GitHub has the merge already (the PUT landed before the stop).
		merged bool
		want   dbstate.MergeState
		puts   int
	}{
		{"stopped after the evaluation began", "evaluating", false, dbstate.MergeMerged, 1},
		{"stopped with merging committed", "merging", false, dbstate.MergeBlocked, 0},
		{"stopped with merging committed, PUT landed", "merging", true, dbstate.MergeMerged, 0},
		{"stopped uncertain", "uncertain", false, dbstate.MergeBlocked, 0},
		{"stopped uncertain, PUT landed", "uncertain", true, dbstate.MergeMerged, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			// No loop is running, so the call returns with the evaluation begun and nothing sent.
			e.svc.Settle = 50 * time.Millisecond
			v, err := e.svc.Merge(ctx, e.input("r1"))
			if err != nil || v.State != dbstate.MergeEvaluating || e.puts() != 0 {
				t.Fatalf("view = %+v, %v, puts %d", v, err, e.puts())
			}
			if tc.state != "evaluating" {
				e.exec(`UPDATE merge_candidates SET state = $2 WHERE id = (SELECT candidate_id FROM merge_proposals WHERE id = $1)`, v.ProposalID, tc.state)
			}
			if tc.merged {
				e.fake.Update("o", "n", func(r *githubtest.Repository) {
					r.PullRequests[0].Merged, r.PullRequests[0].Closed = true, true
					r.PullRequests[0].MergeCommitSHA = "3333333333333333333333333333333333333333"
				})
			}
			// A merge that never ran is only called not applied once the window has passed.
			e.now.Store(int64((2 * time.Minute).Seconds()))
			e.run()
			if got := e.settled(v.ProposalID); got != tc.want || e.puts() != tc.puts {
				t.Fatalf("state = %s (%s), puts %d", got, e.detail(v.ProposalID), e.puts())
			}
			// The call is replayed: it reports the stored outcome and sends nothing more.
			again, err := e.svc.Merge(ctx, e.input("r1"))
			if err != nil || again.State != tc.want || e.puts() != tc.puts {
				t.Fatalf("replay = %+v, %v, puts %d", again, err, e.puts())
			}
		})
	}
}

func TestOnlyOneCandidateHoldsTheMergeIntent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	v, err := e.svc.Prepare(ctx, e.input("r1"))
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE merge_candidates SET state = 'merging' WHERE id = (SELECT candidate_id FROM merge_proposals WHERE id = $1)`, v.ProposalID)
	_, err = e.svc.Prepare(ctx, e.input("r2"))
	var ref *merge.Refusal
	if !errors.As(err, &ref) || ref.Code != "merge_in_progress" {
		t.Fatalf("err = %v", err)
	}
	// And the database refuses a second one outright.
	e.exec(`UPDATE merge_candidates SET state = 'prepared' WHERE state = 'merging'`)
	a, _ := e.svc.Prepare(ctx, e.input("r3"))
	b, _ := e.svc.Prepare(ctx, e.input("r4"))
	e.exec(`UPDATE merge_candidates SET state = 'merging' WHERE id = (SELECT candidate_id FROM merge_proposals WHERE id = $1)`, b.ProposalID)
	_ = a
	if _, err := e.pool.Exec(ctx, `UPDATE merge_candidates SET state = 'merging' WHERE state = 'superseded'`); err == nil {
		t.Fatal("two candidates held the merge intent")
	}
}

// consent records the owner's approval of an exact merge call, as the HITL package stores it.
func (e *env) consent(proposal, request string, mut func(*consentSpec)) {
	e.t.Helper()
	spec := consentSpec{
		Session: "s1", Name: "mcp__mainloop_merge__" + config.MergeToolName, Approved: true, State: "accepted",
		Args: map[string]any{"proposal_id": proposal, "request_id": request}, Hash: hashOf(proposal, request),
	}
	if mut != nil {
		mut(&spec)
	}
	id := fmt.Sprintf("req-%s-%s", proposal[:6], request)
	req := map[string]any{"type": "tool_approval", "hint": "h", "nested": spec.Nested, "tools": []map[string]any{{
		"id": "t1", "call_id": "c1", "name": spec.Name, "args": spec.Args,
	}}}
	reqJSON, _ := json.Marshal(req)
	receipt, _ := json.Marshal(map[string]any{"calls": []map[string]any{{
		"approved": spec.Approved, "call_id": "c1", "tool_name": spec.Name, "arguments_hash": spec.Hash,
	}}})
	e.exec(`INSERT INTO hitl_requests (id, owner_id, session_id, kagent_session_id, agent_namespace, agent_name, context_id, task_id, status_message_id, request_hash, kind, payload)
		VALUES ($1, $2, $3, 'k1', 'ns', 'a', 'ctx', $1, 'm', 'h', 'tool_approval', $4)`, id, owner, spec.Session, reqJSON)
	e.exec(`INSERT INTO hitl_responses (id, request_id, owner_id, idempotency_key, decision, decision_hash, outbound_message_id, receipt, state)
		VALUES ($1, $1, $2, $1, '{}', 'h', $1, $3, $4)`, id, owner, receipt, spec.State)
}

type consentSpec struct {
	Session, Name, Hash, State string
	Approved                   bool
	Args                       map[string]any
	Nested                     any
}

func hashOf(proposal, request string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf(`{"proposal_id":%q,"request_id":%q}`, proposal, request)))
	return hex.EncodeToString(sum[:])
}

func (e *env) approvalProposal() merge.View {
	e.t.Helper()
	e.policy("approval")
	v, err := e.svc.Merge(context.Background(), e.input("prep"))
	if err != nil || v.Status != "approval_required" {
		e.t.Fatalf("view = %+v, %v", v, err)
	}
	return v
}

func (e *env) approved(proposal, request string) (merge.View, error) {
	return e.svc.MergeApproved(context.Background(), merge.ApprovedInput{OwnerID: owner, ActorProjectID: "p1", SessionID: "s1", ProposalID: proposal, RequestID: request})
}

func TestApprovedMergeNeedsTheOwnersExactConsent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	v := e.approvalProposal()

	refusal := func(err error) string {
		var r *merge.Refusal
		if errors.As(err, &r) {
			return r.Code
		}
		return fmt.Sprint(err)
	}
	// No consent at all.
	if _, err := e.approved(v.ProposalID, "call1"); refusal(err) != "approval_required" {
		t.Fatalf("no consent: %v", err)
	}
	// Each of these decisions is not consent to this call.
	e.consent(v.ProposalID, "call1", func(s *consentSpec) { s.Approved = false })
	e.consent(v.ProposalID, "call2", func(s *consentSpec) { s.Name = "mcp__other__" + config.MergeToolName })
	e.consent(v.ProposalID, "call3", func(s *consentSpec) { s.Hash = hashOf(v.ProposalID, "different") })
	e.consent(v.ProposalID, "call4", func(s *consentSpec) { s.Nested = map[string]any{"x": 1} })
	e.consent(v.ProposalID, "call5", func(s *consentSpec) { s.Name = "mainloop_merge." + config.MergeToolName }) // a Codex name for a Claude session
	e.consent(v.ProposalID, "call6", func(s *consentSpec) { s.State = "rejected_transport" })
	e.consent(v.ProposalID, "call7", func(s *consentSpec) { s.Args["extra"] = "x" })
	for _, r := range []string{"call1", "call2", "call3", "call4", "call5", "call6", "call7"} {
		if _, err := e.approved(v.ProposalID, r); refusal(err) != "approval_required" {
			t.Errorf("%s: err = %v", r, err)
		}
	}
	if e.puts() != 0 {
		t.Fatal("merged without consent")
	}
	// A matching approval from another proposal's call does not cover this one.
	e.consent("other-proposal-id", "call8", nil)
	if _, err := e.approved(v.ProposalID, "call8"); refusal(err) != "approval_required" {
		t.Fatalf("other proposal: %v", err)
	}
	// The exact approval merges.
	e.consent(v.ProposalID, "good", nil)
	got, err := e.approved(v.ProposalID, "good")
	if err != nil || got.State != dbstate.MergeMerged || e.puts() != 1 {
		t.Fatalf("view = %+v, %v, puts %d", got, err, e.puts())
	}
	// Reusing the call is idempotent; another request id cannot merge it again.
	if again, err := e.approved(v.ProposalID, "good"); err != nil || again.State != dbstate.MergeMerged || e.puts() != 1 {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if _, err := e.approved(v.ProposalID, "call9"); err == nil || e.puts() != 1 {
		t.Fatalf("second call = %v, puts %d", err, e.puts())
	}
}

func TestApprovedMergeFromAnotherSessionIsRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	v := e.approvalProposal()
	e.consent(v.ProposalID, "good", nil)
	_, err := e.svc.MergeApproved(context.Background(), merge.ApprovedInput{OwnerID: owner, ActorProjectID: "p1", SessionID: "s2", ProposalID: v.ProposalID, RequestID: "good"})
	if err == nil || e.puts() != 0 {
		t.Fatalf("err = %v, puts %d", err, e.puts())
	}
}

func TestApprovedMergeOnAnAutoProposalIsRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	v, err := e.svc.Prepare(context.Background(), e.input("r1"))
	if err != nil {
		t.Fatal(err)
	}
	e.consent(v.ProposalID, "good", nil)
	if _, err := e.approved(v.ProposalID, "good"); err == nil {
		t.Fatal("an auto proposal was executed through the approval surface")
	}
}

func TestWithdrawnConsentBlocksTheEvaluation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	v := e.approvalProposal()
	e.consent(v.ProposalID, "good", nil)
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		r.Runs[tip] = []githubtest.CheckRun{{ID: 1, AppID: 9, Name: "ci", Status: "queued"}}
	})
	e.svc.Settle = 100 * time.Millisecond
	got, err := e.approved(v.ProposalID, "good")
	if err != nil || got.State != dbstate.MergeEvaluating {
		t.Fatalf("view = %+v, %v", got, err)
	}
	// The answer never reached the agent: the consent is no longer live.
	e.exec(`UPDATE hitl_responses SET state = 'rejected_transport'`)
	if s := e.settled(v.ProposalID); s != dbstate.MergeBlocked || e.detail(v.ProposalID) != "consent_withdrawn" || e.puts() != 0 {
		t.Fatalf("state = %s (%s), puts %d", s, e.detail(v.ProposalID), e.puts())
	}
}

func TestCompletionSweepMergesAnApprovedProposalWhoseCallNeverArrived(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	v := e.approvalProposal()
	if e.puts() != 0 {
		t.Fatal("merged before consent")
	}
	e.consent(v.ProposalID, "good", nil)
	if got := e.settled(v.ProposalID); got != dbstate.MergeMerged || e.puts() != 1 {
		t.Fatalf("state = %s, puts %d", got, e.puts())
	}
	// The call that finally arrives finds the finished merge and sends nothing.
	if again, err := e.approved(v.ProposalID, "good"); err != nil || again.State != dbstate.MergeMerged || e.puts() != 1 {
		t.Fatalf("late call = %+v, %v, puts %d", again, err, e.puts())
	}
}

func TestStatusReadsStoredStateOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	ctx := context.Background()
	v, err := e.svc.Merge(ctx, e.input("r1"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.Status(ctx, owner, "p1", v.ProposalID, "r1")
	if err != nil || got.State != dbstate.MergeMerged || got.ProposalID != v.ProposalID {
		t.Fatalf("status = %+v, %v", got, err)
	}
	if _, err := e.svc.Status(ctx, owner, "p1", v.ProposalID, "unknown"); !errors.Is(err, merge.ErrNotFound) {
		t.Fatalf("unknown request id = %v", err)
	}
	if _, err := e.svc.Status(ctx, "someone-else", "p1", v.ProposalID, "r1"); !errors.Is(err, merge.ErrNotFound) {
		t.Fatalf("foreign owner = %v", err)
	}
	if e.puts() != 1 {
		t.Fatalf("puts = %d", e.puts())
	}
}

// planMessage is the one 403 body that means the plan has no protection or rules feature.
const planMessage = "Upgrade to GitHub Pro or make this repository public to enable this feature."

func TestRulesUnavailableOnPlanKeepsCIEvidence(t *testing.T) {
	t.Parallel(
	// A 403 on the protection and rules reads means the plan has neither: the checks decide.
	)

	e := newEnv(t)
	e.run()
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		r.ProtectionStatus, r.RulesStatus = 403, 403
		r.ProtectionMessage, r.RulesMessage = planMessage, planMessage
	})
	v, err := e.svc.Merge(context.Background(), e.input("r1"))
	if err != nil || v.State != dbstate.MergeMerged {
		t.Fatalf("view = %+v, %v", v, err)
	}
}

func TestOtherForbiddenReadsAreNotTreatedAsNoPlan(t *testing.T) {
	t.Parallel(
	// A 403 on the check-run read is a failure to read CI, not an absent feature: nothing merges.
	)

	e := newEnv(t)
	e.run()
	e.fake.Fail("GET", "/repos/o/n/commits/"+tip+"/check-runs", 403)
	_, err := e.svc.Prepare(context.Background(), e.input("r1"))
	var ref *merge.Refusal
	if !errors.As(err, &ref) || ref.Code != "github_unavailable" || e.puts() != 0 {
		t.Fatalf("err = %v", err)
	}
}

func TestRulesReadFailureOtherThan403IsNotCIOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.fake.Update("o", "n", func(r *githubtest.Repository) { r.RulesStatus = 500 })
	_, err := e.svc.Prepare(context.Background(), e.input("r1"))
	var ref *merge.Refusal
	if !errors.As(err, &ref) || ref.Code != "github_unavailable" {
		t.Fatalf("err = %v", err)
	}
}

func TestRequiredCheckWithoutResultWaits(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		r.Protection = &githubtest.Protection{RequiredChecks: []string{"deploy"}}
	})
	e.svc.Settle = 150 * time.Millisecond
	v, err := e.svc.Merge(context.Background(), e.input("r1"))
	if err != nil || v.State != dbstate.MergeEvaluating || e.puts() != 0 {
		t.Fatalf("view = %+v, %v, puts %d", v, err, e.puts())
	}
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		r.Runs[tip] = append(r.Runs[tip], githubtest.CheckRun{ID: 2, AppID: 9, Name: "deploy", Conclusion: "success"})
	})
	if got := e.settled(v.ProposalID); got != dbstate.MergeMerged {
		t.Fatalf("state = %s", got)
	}
}

func TestMergeQueueRoutesToApproval(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.fake.Update("o", "n", func(r *githubtest.Repository) { r.Rules = []githubtest.Rule{{Type: "merge_queue"}} })
	v, err := e.svc.Merge(context.Background(), e.input("r1"))
	if err != nil || v.Status != "approval_required" || len(v.Summary.UnavailableReasons) == 0 {
		t.Fatalf("view = %+v, %v", v, err)
	}
}

// runtimeFake is the trusted kagent read: which Agent each Session runs on and which named template
// each Agent has. A missing template entry reads as an inline-template Agent.
type runtimeFake struct {
	mu        sync.Mutex
	agents    map[string]kagent.AgentRef
	templates map[kagent.AgentRef]string
	err       error
}

func newRuntimeFake() *runtimeFake {
	ref := kagent.AgentRef{Namespace: "ns", Name: "a"}
	return &runtimeFake{agents: map[string]kagent.AgentRef{"k1": ref}, templates: map[kagent.AgentRef]string{ref: "t"}}
}

func (r *runtimeFake) SessionAgent(_ context.Context, id string) (kagent.AgentRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return kagent.AgentRef{}, r.err
	}
	ref, ok := r.agents[id]
	if !ok {
		return kagent.AgentRef{}, errors.New("no such session")
	}
	return ref, nil
}

func (r *runtimeFake) AgentTemplate(_ context.Context, ref kagent.AgentRef) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return "", r.err
	}
	t, ok := r.templates[ref]
	if !ok {
		return "", kagent.ErrNoTemplate
	}
	return t, nil
}

func (r *runtimeFake) template(ref kagent.AgentRef, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if name == "" {
		delete(r.templates, ref)
		return
	}
	r.templates[ref] = name
}

// A bare Service returns at once, so the constructor the processes use must set the wait the spec
// promises; without it merge_pull_request answers "evaluating" before the loop has run.
func TestNewServiceWaitsForTheLoopToSettle(t *testing.T) {
	t.Parallel()
	if got := merge.NewService(&config.Config{}, nil, nil, nil, nil).Settle; got != merge.DefaultSettle || got < 10*time.Second {
		t.Fatalf("Settle = %v, want %v", got, merge.DefaultSettle)
	}
}
