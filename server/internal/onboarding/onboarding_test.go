package onboarding_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/attention"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/environments"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/onboarding"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

const owner = "owner-1"

type fakeGitHub struct {
	installed bool
	err       error
	asked     []string
}

func (f *fakeGitHub) Installed(_ context.Context, repo github.Repo) (bool, error) {
	f.asked = append(f.asked, repo.FullName())
	return f.installed, f.err
}

type fakeImages struct {
	err   error
	asked []string
}

func (f *fakeImages) Validate(_ context.Context, ref, arch, _, _ string) (dbq.InsertEnvironmentVersionParams, error) {
	f.asked = append(f.asked, ref+" "+arch)
	if f.err != nil {
		return dbq.InsertEnvironmentVersionParams{}, f.err
	}
	digest := "sha256:" + strings.Repeat("b", 64)
	return dbq.InsertEnvironmentVersionParams{PlatformManifestDigest: &digest}, nil
}

type env struct {
	svc    *onboarding.Service
	inbox  *attention.Service
	gh     *fakeGitHub
	images *fakeImages
	db     *testdb.DB
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d := testdb.New(t)
	e := &env{gh: &fakeGitHub{installed: true}, images: &fakeImages{}, db: d}
	e.svc = &onboarding.Service{Pool: d.Pool, OwnerID: owner, GitHub: e.gh, Images: e.images}
	e.inbox = &attention.Service{Pool: d.Pool, OwnerID: owner}
	return e
}

func (e *env) items(t *testing.T) []api.AttentionItem {
	t.Helper()
	snap, err := e.inbox.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return snap.Items
}

func (e *env) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.db.Pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

var main = onboarding.Proposer{Role: "main", SessionID: ptr("main-session")}

func ptr[T any](v T) *T { return &v }

func fullSpec() onboarding.Spec {
	return onboarding.Spec{
		Repository:       "https://github.com/oldsj/opencozy-next",
		Why:              "Pilot: run OpenCozy UI work through Mainloop.",
		EnvironmentImage: "ghcr.io/oldsj/opencozy-env@sha256:" + strings.Repeat("a", 64),
		VerificationMode: "ci",
		WorkspaceService: &api.WorkspaceService{Command: []string{"pnpm", "dev"}, Ports: []api.WorkspacePort{{Name: "web", Number: 5173}}},
		Services:         []string{"postgres:16"},
		WorkspaceSize:    "large",
	}
}

// Given an installed App and a valid image, when main proposes a repository, then one proposal
// waits and one approval card with Approve and Reject is in Needs you, saying what was found and
// what is recorded but not applied.
func TestProposeOpensOneApprovalCard(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	p, err := e.svc.Propose(ctx, main, "req-1", fullSpec())
	if err != nil {
		t.Fatal(err)
	}
	if p.State != onboarding.StateProposed || p.Repository != "oldsj/opencozy-next" || p.Replayed ||
		p.Facts.GitHubApp != onboarding.AppInstalled || p.Facts.ProjectID != nil ||
		p.Facts.EnvironmentDigest != "sha256:"+strings.Repeat("b", 64) || p.Spec.EnvironmentArchitecture != "amd64" {
		t.Fatalf("proposal = %+v", p)
	}
	if len(e.gh.asked) != 1 || e.gh.asked[0] != "oldsj/opencozy-next" || len(e.images.asked) != 1 {
		t.Fatalf("checks: github %v, images %v", e.gh.asked, e.images.asked)
	}
	items := e.items(t)
	if len(items) != 1 {
		t.Fatalf("items = %+v", items)
	}
	it := items[0]
	if it.Kind != api.AttentionItemKindApproval || it.Title != "Onboard oldsj/opencozy-next" || it.QueueItem == nil ||
		it.QueueItem.Id != p.QueueItemID || it.QueueItem.Options == nil ||
		strings.Join(*it.QueueItem.Options, ",") != "Approve,Reject" {
		t.Fatalf("item = %+v", it)
	}
	for _, want := range []string{
		"Main proposes making oldsj/opencozy-next a Mainloop project.",
		"GitHub App: installed",
		"Project: new; approving imports it.",
		"Checks: ci.",
		"Workspace service: pnpm dev; ports web=5173.",
		"Recorded, not applied (Mainloop has no declared services or workspace sizes yet): services postgres:16; workspace size large.",
	} {
		if !strings.Contains(it.QueueItem.Content, want) {
			t.Errorf("card text lacks %q:\n%s", want, it.QueueItem.Content)
		}
	}
	if got := it.QueueItem.Context["project_proposal_id"]; got != p.ID {
		t.Errorf("card context = %v", it.QueueItem.Context)
	}
}

// Given a proposal, when the same request id comes again, then it reads the same proposal back
// and opens no second card; a different proposal under that request id is a conflict.
func TestProposeIsIdempotentOnRequestID(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	first, err := e.svc.Propose(ctx, main, "req-1", fullSpec())
	if err != nil {
		t.Fatal(err)
	}

	again, err := e.svc.Propose(ctx, main, "req-1", fullSpec())
	if err != nil || again.ID != first.ID || !again.Replayed {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if len(e.gh.asked) != 1 {
		t.Errorf("a replay asked GitHub again: %v", e.gh.asked)
	}
	changed := fullSpec()
	changed.VerificationMode = "local"
	if _, err := e.svc.Propose(ctx, main, "req-1", changed); !errors.Is(err, onboarding.ErrConflict) {
		t.Fatalf("changed replay err = %v", err)
	}
	if n := e.count(t, `SELECT count(*) FROM project_proposals`); n != 1 {
		t.Errorf("proposals = %d", n)
	}
	if items := e.items(t); len(items) != 1 {
		t.Errorf("items = %+v", items)
	}
}

// Given an open proposal, when another request proposes the same repository in another case,
// then it gets the open proposal back and no second card.
func TestProposeReturnsTheRepositorysOpenProposal(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	first, err := e.svc.Propose(ctx, main, "req-1", onboarding.Spec{Repository: "oldsj/opencozy-next", Why: "pilot"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.svc.Propose(ctx, main, "req-2", onboarding.Spec{Repository: "OldsJ/OpenCozy-Next", Why: "again"})
	if err != nil || second.ID != first.ID || !second.Replayed {
		t.Fatalf("second = %+v, %v", second, err)
	}
	if items := e.items(t); len(items) != 1 {
		t.Errorf("items = %+v", items)
	}
}

// Given the App is not installed, when a supervisor proposes, then the proposal still waits, and
// its card and facts tell the owner to install the App first.
func TestProposeSaysWhenTheAppIsNotInstalled(t *testing.T) {
	e := newEnv(t)
	e.gh.installed = false
	sup := onboarding.Proposer{Role: "supervisor", SessionID: ptr("s1"), TaskID: ptr("task-9")}

	p, err := e.svc.Propose(context.Background(), sup, "req-1", onboarding.Spec{Repository: "oldsj/opencozy-next", Why: "pilot"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Facts.GitHubApp != onboarding.AppNotInstalled {
		t.Fatalf("facts = %+v", p.Facts)
	}
	it := e.items(t)[0]
	if !strings.Contains(it.Needed, "The GitHub App is not installed on it yet.") ||
		!strings.Contains(it.QueueItem.Content, "Supervisor task task-9 proposes") ||
		!strings.Contains(it.QueueItem.Content, "GitHub App: NOT installed. "+onboarding.InstallHint("oldsj/opencozy-next")) ||
		!strings.Contains(it.QueueItem.Content, "Environment: unchanged") {
		t.Fatalf("item = %+v\n%s", it, it.QueueItem.Content)
	}
}

// Given GitHub cannot be asked, when main proposes, then the installation is unknown and the card
// says why, rather than the proposal failing.
func TestProposeRecordsAnUnreachableGitHubAsUnknown(t *testing.T) {
	e := newEnv(t)
	e.gh.err = github.ErrUnavailable
	p, err := e.svc.Propose(context.Background(), main, "req-1", onboarding.Spec{Repository: "oldsj/x", Why: "pilot"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Facts.GitHubApp != onboarding.AppUnknown || !strings.Contains(p.Facts.GitHubAppDetail, "GitHub could not be asked") {
		t.Fatalf("facts = %+v", p.Facts)
	}
}

// Given the repository is already a project, when main proposes it, then the card says the
// settings apply to the existing project.
func TestProposeNamesAnExistingProject(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO projects (id, user_id, owner, name, full_name, html_url)
		VALUES ('p1', $1, 'oldsj', 'opencozy-next', 'oldsj/opencozy-next', 'https://github.com/oldsj/opencozy-next')`, owner); err != nil {
		t.Fatal(err)
	}
	p, err := e.svc.Propose(ctx, main, "req-1", onboarding.Spec{Repository: "OLDSJ/opencozy-next", Why: "settings"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Facts.ProjectID == nil || *p.Facts.ProjectID != "p1" {
		t.Fatalf("facts = %+v", p.Facts)
	}
	if c := e.items(t)[0].QueueItem.Content; !strings.Contains(c, "Project: already a project") {
		t.Fatalf("card:\n%s", c)
	}
}

// Given an image the static check refuses, when main proposes it, then the proposal fails with the
// registry's reason and nothing is stored or put to the owner.
func TestProposeRefusesAnImageTheCheckRefuses(t *testing.T) {
	e := newEnv(t)
	e.images.err = &environments.RegistryError{Message: "image config USER must be 65532:65532"}
	_, err := e.svc.Propose(context.Background(), main, "req-1", fullSpec())
	if !errors.Is(err, onboarding.ErrInvalid) || !strings.Contains(err.Error(), "USER must be 65532:65532") {
		t.Fatalf("err = %v", err)
	}
	if n := e.count(t, `SELECT count(*) FROM project_proposals`); n != 0 {
		t.Errorf("proposals = %d", n)
	}
	if items := e.items(t); len(items) != 0 {
		t.Errorf("items = %+v", items)
	}
}

// Given a proposal that breaks the contract, when it is made, then it is refused before anything is
// read or written.
func TestProposeValidates(t *testing.T) {
	e := newEnv(t)
	cases := map[string]func(*onboarding.Spec){
		"bad repository":         func(s *onboarding.Spec) { s.Repository = "not a repo" },
		"no why":                 func(s *onboarding.Spec) { s.Why = "  " },
		"bad verification":       func(s *onboarding.Spec) { s.VerificationMode = "sometimes" },
		"bad architecture":       func(s *onboarding.Spec) { s.EnvironmentArchitecture = "riscv" },
		"architecture, no image": func(s *onboarding.Spec) { s.EnvironmentImage = ""; s.EnvironmentArchitecture = "arm64" },
		"too many services":      func(s *onboarding.Spec) { s.Services = strings.Split("a,b,c,d,e,f,g,h,i", ",") },
		"multi-line service":     func(s *onboarding.Spec) { s.Services = []string{"postgres\nrm -rf"} },
		"bad port": func(s *onboarding.Spec) {
			s.WorkspaceService = &api.WorkspaceService{Command: []string{"x"}, Ports: []api.WorkspacePort{{Name: "Web", Number: 0}}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			spec := fullSpec()
			mutate(&spec)
			if _, err := e.svc.Propose(context.Background(), main, "req-"+name, spec); !errors.Is(err, onboarding.ErrInvalid) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	if _, err := e.svc.Propose(context.Background(), main, "", fullSpec()); !errors.Is(err, onboarding.ErrInvalid) {
		t.Fatalf("empty request id err = %v", err)
	}
	if len(e.gh.asked) != 0 || len(e.images.asked) != 0 {
		t.Errorf("an invalid proposal was checked: %v %v", e.gh.asked, e.images.asked)
	}
}

// Given a proposal's card, when the owner taps Approve, then the queue answer is recorded on it
// (the slice that applies proposals reads it) and the card leaves Needs you.
func TestTheCardTakesTheOwnersAnswer(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p, err := e.svc.Propose(ctx, main, "req-1", onboarding.Spec{Repository: "oldsj/opencozy-next", Why: "pilot"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.inbox.Respond(ctx, p.QueueItemID, api.QueueItemResponse{Response: onboarding.AnswerApprove}); err != nil {
		t.Fatal(err)
	}
	if items := e.items(t); len(items) != 0 {
		t.Fatalf("items = %+v", items)
	}
	if n := e.count(t, `SELECT count(*) FROM attention_items WHERE source_id = $1 AND detail->>'response' = 'Approve'`, p.QueueItemID); n != 1 {
		t.Errorf("answered cards = %d", n)
	}
}
