package projects_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
	"github.com/oldsj/mainloop/server/internal/projects"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

const owner = "owner-1"

func newGitHub(t *testing.T) (*githubtest.Server, projects.Remote) {
	t.Helper()
	fake := githubtest.New(t)
	desc := "the loop"
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	fake.AddRepo(githubtest.Repository{
		Owner: "oldsj", Name: "mainloop", Description: &desc, DefaultBranch: "trunk", AvatarURL: "https://a.example/o.png",
		Issues: 2,
		PullRequests: []githubtest.PullRequest{
			{Number: 2, Title: "two", Author: "o", HeadRef: "mainloop/x", CreatedAt: now, UpdatedAt: now},
			{Number: 1, Title: "one", Author: "o", HeadRef: "fix", CreatedAt: now, UpdatedAt: now},
		},
		Commits: []githubtest.Commit{{SHA: "abcdef0123", Message: "first\n\nmore", Author: "A", Date: now}},
	})
	app, err := github.NewApp(github.Options{AppID: fmt.Sprint(githubtest.AppID), PrivateKey: githubtest.PrivateKey(), BaseURL: fake.URL})
	if err != nil {
		t.Fatal(err)
	}
	return fake, app
}

func newService(t *testing.T, remote projects.Remote, writes bool) *projects.Service {
	t.Helper()
	return &projects.Service{
		Pool: testdb.New(t).Pool, Remote: remote, OwnerID: owner, PolicyWrites: writes,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// Mined from backend/tests/test_projects.py (import is idempotent per repository, case-insensitive).
func TestImportIsIdempotentAndCaseInsensitive(t *testing.T) {
	svc := newService(t, nil, false)
	ctx := context.Background()
	first, err := svc.Import(ctx, "oldsj/mainloop")
	if err != nil {
		t.Fatal(err)
	}
	if first.FullName != "oldsj/mainloop" || first.HTMLURL != "https://github.com/oldsj/mainloop" ||
		first.MergePolicy != dbstate.MergeAuto || first.MergePolicyVersion != 1 || first.UserID != owner {
		t.Fatalf("%+v", first)
	}
	for _, text := range []string{"OldSJ/Mainloop", "https://github.com/oldsj/mainloop.git", " oldsj/mainloop/ "} {
		again, err := svc.Import(ctx, text)
		if err != nil || again.ID != first.ID {
			t.Fatalf("%q: %v id %s vs %s", text, err, again.ID, first.ID)
		}
		if !again.LastUsedAt.After(first.LastUsedAt) {
			t.Fatalf("%q: last_used_at not touched", text)
		}
	}
	list, err := svc.List(ctx, 20)
	if err != nil || len(list) != 1 {
		t.Fatalf("%v %d", err, len(list))
	}
}

func TestImportRejectsBadRepos(t *testing.T) {
	svc := newService(t, nil, false)
	for _, bad := range []string{"", "x", "https://gitlab.com/a/b", "a/b/c", "http://github.com/a/b"} {
		if _, err := svc.Import(context.Background(), bad); !errors.Is(err, projects.ErrInvalidRepo) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if list, _ := svc.List(context.Background(), 20); len(list) != 0 {
		t.Fatal("a rejected import left a project")
	}
}

func TestImportFillsMetadataFromGitHub(t *testing.T) {
	_, remote := newGitHub(t)
	svc := newService(t, remote, false)
	p, err := svc.Import(context.Background(), "OLDSJ/MainLoop")
	if err != nil {
		t.Fatal(err)
	}
	// GitHub's own spelling replaces what the owner typed.
	if p.FullName != "oldsj/mainloop" || p.DefaultBranch != "trunk" || p.OpenPRCount != 2 || p.OpenIssueCount != 2 ||
		p.Description == nil || *p.Description != "the loop" || p.AvatarURL == nil || p.MetadataUpdatedAt == nil {
		t.Fatalf("%+v", p)
	}
}

func TestImportSurvivesGitHubFailure(t *testing.T) {
	fake, remote := newGitHub(t)
	svc := newService(t, remote, false)
	fake.Fail("GET", "/repos/oldsj/mainloop", 500)
	p, err := svc.Import(context.Background(), "oldsj/mainloop")
	if err != nil || p.MetadataUpdatedAt != nil || p.DefaultBranch != "" {
		t.Fatalf("%+v %v", p, err)
	}
	// A repository the App is not installed on is the same: the project is recorded.
	q, err := svc.Import(context.Background(), "someone/else")
	if err != nil || q.FullName != "someone/else" {
		t.Fatalf("%+v %v", q, err)
	}
	fake.Fail("GET", "/repos/oldsj/mainloop", 0)
	p, err = svc.Import(context.Background(), "oldsj/mainloop")
	if err != nil || p.MetadataUpdatedAt == nil {
		t.Fatalf("retry did not fill metadata: %+v %v", p, err)
	}
}

func TestListOrdersByLastUsedAndLimits(t *testing.T) {
	svc := newService(t, nil, false)
	ctx := context.Background()
	for _, r := range []string{"a/one", "a/two", "a/three"} {
		if _, err := svc.Import(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Import(ctx, "a/one"); err != nil { // touch
		t.Fatal(err)
	}
	list, _ := svc.List(ctx, 20)
	if len(list) != 3 || list[0].FullName != "a/one" || list[2].FullName != "a/two" {
		t.Fatalf("order: %s %s %s", list[0].FullName, list[1].FullName, list[2].FullName)
	}
	if two, _ := svc.List(ctx, 2); len(two) != 2 {
		t.Fatal("limit ignored")
	}
	if none, err := svc.List(ctx, 0); err != nil || len(none) != 0 {
		t.Fatal("limit 0")
	}
	if none, err := svc.List(ctx, -3); err != nil || len(none) != 0 {
		t.Fatal("negative limit")
	}
}

func TestGetIsScopedToTheOwner(t *testing.T) {
	svc := newService(t, nil, false)
	p, _ := svc.Import(context.Background(), "a/b")
	other := *svc
	other.OwnerID = "someone-else"
	if _, err := other.Get(context.Background(), p.ID); !errors.Is(err, projects.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if _, err := svc.Get(context.Background(), "nope"); !errors.Is(err, projects.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if _, err := svc.Detail(context.Background(), "nope"); !errors.Is(err, projects.ErrNotFound) {
		t.Fatalf("detail: %v", err)
	}
	if _, err := svc.MergePolicy(context.Background(), "nope"); !errors.Is(err, projects.ErrNotFound) {
		t.Fatalf("policy: %v", err)
	}
}

func TestDetailShowsGitHubDataAndSessions(t *testing.T) {
	fake, remote := newGitHub(t)
	svc := newService(t, remote, false)
	ctx := context.Background()
	p, err := svc.Import(ctx, "oldsj/mainloop")
	if err != nil {
		t.Fatal(err)
	}
	insertSession(t, svc, "s1", p.ID, false)
	insertSession(t, svc, "s2", p.ID, true) // archived: not listed
	insertSession(t, svc, "main", p.ID, false)
	if _, err := svc.Pool.Exec(ctx, `INSERT INTO agent_bindings (session_id, kind, role) VALUES ('main', 'claude', 'main')`); err != nil {
		t.Fatal(err)
	}
	d, err := svc.Detail(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.PRs) != 2 || d.PRs[0].Number != 2 || !d.PRs[0].IsMainloop || d.PRs[1].IsMainloop {
		t.Fatalf("prs %+v", d.PRs)
	}
	if len(d.Commits) != 1 || d.Commits[0].SHA != "abcdef0" || d.Commits[0].Message != "first" {
		t.Fatalf("commits %+v", d.Commits)
	}
	if len(d.Sessions) != 1 || d.Sessions[0].ID != "s1" {
		t.Fatalf("sessions %+v", d.Sessions)
	}
	// The detail asks for each permission set separately and never for more than it reads.
	for _, m := range fake.Mints() {
		if len(m.Repositories) != 1 || m.Repositories[0] != "mainloop" {
			t.Errorf("mint %+v", m)
		}
		for _, level := range m.Permissions {
			if level != "read" {
				t.Errorf("mint %+v asks for more than read", m)
			}
		}
	}
	for _, r := range fake.Requests() {
		if r.Method != "GET" && r.Method != "POST" {
			t.Errorf("unexpected %s %s", r.Method, r.Path)
		}
	}
}

func TestDetailDegradesToEmptyListsWhenGitHubFails(t *testing.T) {
	fake, remote := newGitHub(t)
	svc := newService(t, remote, false)
	ctx := context.Background()
	p, _ := svc.Import(ctx, "oldsj/mainloop")
	fake.Fail("GET", "/repos/oldsj/mainloop/pulls", 500)
	fake.Fail("GET", "/repos/oldsj/mainloop/commits", 500)
	d, err := svc.Detail(ctx, p.ID)
	if err != nil || d.PRs == nil || len(d.PRs) != 0 || d.Commits == nil || len(d.Commits) != 0 {
		t.Fatalf("%+v %v", d, err)
	}
	noRemote := newService(t, nil, false)
	p2, _ := noRemote.Import(ctx, "a/b")
	d, err = noRemote.Detail(ctx, p2.ID)
	if err != nil || d.PRs == nil || d.Commits == nil {
		t.Fatalf("%+v %v", d, err)
	}
}

func insertSession(t *testing.T, svc *projects.Service, id, project string, archived bool) {
	t.Helper()
	q := `INSERT INTO sessions (id, user_id, project_id, title, description, prompt, status, archived_at)
	      VALUES ($1, $2, $3, 'title', 'desc', 'prompt', 'active', CASE WHEN $4 THEN now() END)`
	if _, err := svc.Pool.Exec(context.Background(), q, id, owner, project, archived); err != nil {
		t.Fatal(err)
	}
}

func TestSetMergePolicy(t *testing.T) {
	ctx := context.Background()
	svc := newService(t, nil, true)
	p, _ := svc.Import(ctx, "a/b")

	m, err := svc.SetMergePolicy(ctx, p.ID, dbstate.MergeApproval, 1)
	if err != nil || m.Policy != dbstate.MergeApproval || m.Version != 2 {
		t.Fatalf("%+v %v", m, err)
	}
	// A stale version is a conflict, whatever the target.
	for _, pol := range []dbstate.MergePolicy{dbstate.MergeAuto, dbstate.MergeApproval} {
		if _, err := svc.SetMergePolicy(ctx, p.ID, pol, 1); !errors.Is(err, projects.ErrVersionConflict) {
			t.Fatalf("stale %s: %v", pol, err)
		}
	}
	// The same policy at the current version changes nothing.
	m, err = svc.SetMergePolicy(ctx, p.ID, dbstate.MergeApproval, 2)
	if err != nil || m.Version != 2 {
		t.Fatalf("no-op: %+v %v", m, err)
	}
	m, err = svc.SetMergePolicy(ctx, p.ID, dbstate.MergeAuto, 2)
	if err != nil || m.Policy != dbstate.MergeAuto || m.Version != 3 {
		t.Fatalf("%+v %v", m, err)
	}
	got, _ := svc.MergePolicy(ctx, p.ID)
	if got != m {
		t.Fatalf("read back %+v want %+v", got, m)
	}
	if _, err := svc.SetMergePolicy(ctx, "nope", dbstate.MergeAuto, 1); !errors.Is(err, projects.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := svc.SetMergePolicy(ctx, p.ID, "bogus", 3); !errors.Is(err, projects.ErrInvalidPolicy) {
		t.Fatalf("bogus: %v", err)
	}

	rows, err := svc.Pool.Query(ctx, `SELECT owner_id, old_policy, new_policy, version FROM project_merge_policy_audit ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got2 []string
	for rows.Next() {
		var o, a, b string
		var v int64
		if err := rows.Scan(&o, &a, &b, &v); err != nil {
			t.Fatal(err)
		}
		got2 = append(got2, fmt.Sprintf("%s %s %s %d", o, a, b, v))
	}
	if fmt.Sprint(got2) != "[owner-1 auto approval 2 owner-1 approval auto 3]" {
		t.Fatalf("audit %v", got2)
	}
}

func TestSetMergePolicyIsGatedAndOwnerScoped(t *testing.T) {
	ctx := context.Background()
	off := newService(t, nil, false)
	p, _ := off.Import(ctx, "a/b")
	if _, err := off.SetMergePolicy(ctx, p.ID, dbstate.MergeApproval, 1); !errors.Is(err, projects.ErrWritesDisabled) {
		t.Fatalf("got %v", err)
	}
	if got, _ := off.MergePolicy(ctx, p.ID); got.Version != 1 || got.Policy != dbstate.MergeAuto {
		t.Fatal("a disabled write changed the policy")
	}
	on := *off
	on.PolicyWrites = true
	on.OwnerID = "intruder"
	if _, err := on.SetMergePolicy(ctx, p.ID, dbstate.MergeApproval, 1); !errors.Is(err, projects.ErrNotFound) {
		t.Fatalf("other owner: %v", err)
	}
}

// Concurrent writers with the same expected version: one wins, the rest see a conflict, and
// exactly one audit row is written.
func TestSetMergePolicyCompareAndSwapUnderRace(t *testing.T) {
	ctx := context.Background()
	svc := newService(t, nil, true)
	p, _ := svc.Import(ctx, "a/b")
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() { _, errs[i] = svc.SetMergePolicy(ctx, p.ID, dbstate.MergeApproval, 1) })
	}
	wg.Wait()
	wins, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, projects.ErrVersionConflict):
			conflicts++
		default:
			t.Fatalf("unexpected %v", err)
		}
	}
	if wins != 1 || conflicts != n-1 {
		t.Fatalf("wins %d conflicts %d", wins, conflicts)
	}
	var rows int
	if err := svc.Pool.QueryRow(ctx, `SELECT count(*) FROM project_merge_policy_audit`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("audit rows %d %v", rows, err)
	}
	if got, _ := svc.MergePolicy(ctx, p.ID); got.Version != 2 {
		t.Fatalf("version %d", got.Version)
	}
}

func TestAuditRowsAreImmutable(t *testing.T) {
	ctx := context.Background()
	svc := newService(t, nil, true)
	p, _ := svc.Import(ctx, "a/b")
	if _, err := svc.SetMergePolicy(ctx, p.ID, dbstate.MergeApproval, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Pool.Exec(ctx, `UPDATE project_merge_policy_audit SET new_policy = 'auto'`); err == nil {
		t.Fatal("audit row updated")
	}
	if _, err := svc.Pool.Exec(ctx, `DELETE FROM project_merge_policy_audit`); err == nil {
		t.Fatal("audit row deleted")
	}
}

// stuck is a Remote whose GitHub never answers until the caller gives up.
type stuck struct{ calls atomic.Int32 }

func (r *stuck) Metadata(ctx context.Context, _ github.Repo) (*github.Metadata, error) {
	r.calls.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*stuck) OpenPullRequests(ctx context.Context, _ github.Repo, _ int) ([]github.PullRequest, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*stuck) RecentCommits(ctx context.Context, _ github.Repo, _ string, _ int) ([]github.Commit, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A slow GitHub costs the import one overall deadline, and the project is still recorded.
func TestImportStopsWaitingForSlowGitHub(t *testing.T) {
	remote := &stuck{}
	svc := newService(t, remote, false)
	svc.MetadataTimeout = 100 * time.Millisecond
	start := time.Now()
	p, err := svc.Import(context.Background(), "oldsj/mainloop")
	if err != nil || p.FullName != "oldsj/mainloop" || p.MetadataUpdatedAt != nil {
		t.Fatalf("%+v %v", p, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("import took %v", took)
	}
	if remote.calls.Load() != 1 {
		t.Fatalf("%d metadata calls", remote.calls.Load())
	}
}

// advancer records the policy advances the project service asks for and can refuse them.
type advancer struct {
	mu    sync.Mutex
	calls []string
	err   error
	// seen reads the project's stored default branch at the moment of each call.
	seen func(id string) string
	was  []string
}

func (a *advancer) AdvanceDefaultBranch(_ context.Context, id, branch string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, id+"="+branch)
	if a.seen != nil {
		a.was = append(a.was, a.seen(id))
	}
	return a.err
}

type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func projectRow(t *testing.T, svc *projects.Service, id string) (branch string, fetched bool) {
	t.Helper()
	p, err := svc.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return p.DefaultBranch, p.MetadataUpdatedAt != nil
}

func TestSweepFillsEmptyMetadata(t *testing.T) {
	fake, remote := newGitHub(t)
	svc := newService(t, remote, false)
	fake.Fail("GET", "/repos/oldsj/mainloop", 500)
	p, err := svc.Import(context.Background(), "oldsj/mainloop")
	if err != nil || p.DefaultBranch != "" {
		t.Fatalf("%+v %v", p, err)
	}
	fake.Fail("GET", "/repos/oldsj/mainloop", 0)
	if err := svc.MetadataSweep().Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(context.Background(), p.ID)
	if err != nil || got.DefaultBranch != "trunk" || got.OpenPRCount != 2 || got.MetadataUpdatedAt == nil {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestSweepLeavesFreshMetadataAlone(t *testing.T) {
	fake, remote := newGitHub(t)
	svc := newService(t, remote, false)
	if _, err := svc.Import(context.Background(), "oldsj/mainloop"); err != nil {
		t.Fatal(err)
	}
	before := fake.Count("GET", "/repos/oldsj/mainloop")
	if err := svc.MetadataSweep().Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := fake.Count("GET", "/repos/oldsj/mainloop"); n != before {
		t.Fatalf("fresh project read again: %d -> %d", before, n)
	}
	// Past the stale age it is read again.
	svc.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if err := svc.MetadataSweep().Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := fake.Count("GET", "/repos/oldsj/mainloop"); n == before {
		t.Fatal("stale project not read")
	}
}

// A GitHub error changes nothing, does not stop the other projects, and the failing project is
// asked again only after the pause.
func TestSweepGitHubErrorChangesNothingAndRetriesLater(t *testing.T) {
	fake, remote := newGitHub(t)
	fake.AddRepo(githubtest.Repository{Owner: "oldsj", Name: "other", DefaultBranch: "dev"})
	svc := newService(t, remote, false)
	ctx := context.Background()
	fake.Fail("GET", "/repos/oldsj/mainloop", 500)
	fake.Fail("GET", "/repos/oldsj/other", 500)
	bad, _ := svc.Import(ctx, "oldsj/mainloop")
	good, _ := svc.Import(ctx, "oldsj/other")
	fake.Fail("GET", "/repos/oldsj/other", 0)
	c := &clock{at: time.Now()}
	svc.Now = c.now
	sweep := svc.MetadataSweep()

	if err := sweep.Run(ctx); err != nil {
		t.Fatalf("a GitHub error failed the pass: %v", err)
	}
	if b, fetched := projectRow(t, svc, bad.ID); b != "" || fetched {
		t.Fatalf("failed refresh changed the row: %q %v", b, fetched)
	}
	if b, _ := projectRow(t, svc, good.ID); b != "dev" {
		t.Fatalf("one failing project stopped the others: %q", b)
	}

	fake.Fail("GET", "/repos/oldsj/mainloop", 0)
	asked := fake.Count("GET", "/repos/oldsj/mainloop")
	if err := sweep.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if n := fake.Count("GET", "/repos/oldsj/mainloop"); n != asked {
		t.Fatalf("asked again inside the pause: %d -> %d", asked, n)
	}
	c.add(6 * time.Minute)
	if err := sweep.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if b, fetched := projectRow(t, svc, bad.ID); b != "trunk" || !fetched {
		t.Fatalf("not retried after the pause: %q %v", b, fetched)
	}
}

// The policy advances before the project row; when it cannot, the row is untouched and the next
// pass tries again.
func TestSweepPolicyFailureLeavesProjectUnchanged(t *testing.T) {
	_, remote := newGitHub(t)
	svc := newService(t, remote, false)
	ctx := context.Background()
	p, err := svc.Import(ctx, "oldsj/mainloop")
	if err != nil || p.DefaultBranch != "trunk" {
		t.Fatalf("%+v %v", p, err)
	}
	// GitHub renamed it after the last read.
	if _, err := svc.Pool.Exec(ctx, `UPDATE projects SET default_branch = 'main', metadata_updated_at = now() - interval '2 hours' WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	adv := &advancer{err: errors.New("policy_mismatch")}
	svc.Policy = adv
	c := &clock{at: time.Now()}
	svc.Now = c.now
	sweep := svc.MetadataSweep()
	if err := sweep.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if b, _ := projectRow(t, svc, p.ID); b != "main" {
		t.Fatalf("project advanced past a refused policy: %q", b)
	}
	adv.mu.Lock()
	adv.err = nil
	adv.mu.Unlock()
	c.add(6 * time.Minute)
	if err := sweep.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if b, _ := projectRow(t, svc, p.ID); b != "trunk" {
		t.Fatalf("not retried: %q", b)
	}
	if len(adv.calls) != 2 || adv.calls[1] != p.ID+"=trunk" {
		t.Fatalf("calls %v", adv.calls)
	}
}

func TestMetadataAdvancesPolicyBeforeTheProjectRow(t *testing.T) {
	_, remote := newGitHub(t)
	svc := newService(t, remote, false)
	adv := &advancer{}
	adv.seen = func(id string) string { b, _ := projectRow(t, svc, id); return b }
	svc.Policy = adv
	p, err := svc.Import(context.Background(), "oldsj/mainloop")
	if err != nil || p.DefaultBranch != "trunk" {
		t.Fatalf("%+v %v", p, err)
	}
	if len(adv.calls) != 1 || adv.calls[0] != p.ID+"=trunk" || adv.was[0] != "" {
		t.Fatalf("calls %v, stored branch at the call %v", adv.calls, adv.was)
	}
	// Unchanged default: no policy call.
	if _, err := svc.Import(context.Background(), "oldsj/mainloop"); err != nil || len(adv.calls) != 1 {
		t.Fatalf("%v %v", err, adv.calls)
	}
}

func TestSweepWithoutGitHubIsANoOp(t *testing.T) {
	svc := newService(t, nil, false)
	if _, err := svc.Import(context.Background(), "a/b"); err != nil {
		t.Fatal(err)
	}
	if err := svc.MetadataSweep().Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}
