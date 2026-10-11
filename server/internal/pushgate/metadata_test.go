package pushgate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
	"github.com/oldsj/mainloop/server/internal/projects"
)

// metadataSweep is the production sweep of the project-metadata reconciler, reading the env's
// fake GitHub and advancing the env's push policy.
func (e *env) metadataSweep() func(context.Context) error {
	e.t.Helper()
	app, err := github.NewApp(github.Options{AppID: fmt.Sprint(githubtest.AppID), PrivateKey: githubtest.PrivateKey(), BaseURL: e.fake.URL})
	if err != nil {
		e.t.Fatal(err)
	}
	svc := &projects.Service{Pool: e.pool, Remote: app, OwnerID: "owner", Policy: e.svc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return svc.MetadataSweep().Run
}

func (e *env) policy() (Policy, int64) {
	e.t.Helper()
	row, err := dbq.New(e.pool).GetPushPolicy(context.Background(), "p")
	if err != nil {
		e.t.Fatal(err)
	}
	var p Policy
	if err = json.Unmarshal(row.Policy, &p); err != nil {
		e.t.Fatal(err)
	}
	return p, row.Version
}

// A project that came in without GitHub metadata (a repo URL, or an import during a GitHub blip)
// is refused every push until the sweep fills its default branch; afterwards the same
// credential publishes.
func TestEmptyDefaultBranchGainsPushAuthorityAfterSweep(t *testing.T) {
	e := newEnv(t)
	e.exec(`UPDATE projects SET default_branch = '', metadata_updated_at = NULL`)
	body := e.body(zeroOID, e.head, "refs/heads/feature/test")
	if _, err := e.svc.Enroll(context.Background(), "ws"); err == nil || !strings.Contains(err.Error(), "metadata_unavailable") {
		t.Fatalf("enroll with no project metadata: %v", err)
	}
	if w := e.request(body, nil); w.Code == 200 || !strings.HasPrefix(w.Body.String(), "metadata_unavailable ") {
		t.Fatalf("push before the sweep: %d %s", w.Code, w.Body.String())
	}
	if err := e.metadataSweep()(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Enroll(context.Background(), "ws"); err != nil {
		t.Fatalf("enroll after the sweep: %v", err)
	}
	if w := e.request(body, nil); w.Code != 200 {
		t.Fatalf("push after the sweep: %d %s", w.Code, w.Body.String())
	}
	if p, _ := e.policy(); p.DefaultBranch != "main" || len(p.PreviousDefaults) != 0 {
		t.Fatalf("policy changed though the default did not: %+v", p)
	}
}

// GitHub renames the default branch to the branch a workspace pushes to. The sweep advances the
// push policy with a compare-and-swap, keeps the old default protected, and pushes to the new
// default are refused as default-branch pushes.
func TestRenamedDefaultBranchAdvancesPolicyAndRefusesNewDefault(t *testing.T) {
	e := newEnv(t)
	e.exec(`UPDATE projects SET metadata_updated_at = now() - interval '1 hour'`)
	_, version := e.policy()
	e.fake.Update("oldsj", "alpha", func(r *githubtest.Repository) { r.DefaultBranch = "feature/test" })
	body := e.body(zeroOID, e.head, "refs/heads/feature/test")
	if w := e.request(body, nil); w.Code == 200 {
		t.Fatalf("push accepted while GitHub, policy and project disagree: %s", w.Body.String())
	}
	if err := e.metadataSweep()(context.Background()); err != nil {
		t.Fatal(err)
	}
	p, after := e.policy()
	if p.DefaultBranch != "feature/test" || len(p.PreviousDefaults) != 1 || p.PreviousDefaults[0] != "main" || after != version+1 {
		t.Fatalf("policy %+v version %d -> %d", p, version, after)
	}
	var stored string
	if err := e.pool.QueryRow(context.Background(), `SELECT default_branch FROM projects WHERE id = 'p'`).Scan(&stored); err != nil || stored != "feature/test" {
		t.Fatalf("project default branch %q: %v", stored, err)
	}
	if w := e.request(body, nil); w.Code == 200 || !strings.HasPrefix(w.Body.String(), "default_branch ") {
		t.Fatalf("push to the new default: %d %s", w.Code, w.Body.String())
	}
	// A second sweep finds nothing to change: the policy version does not move.
	e.exec(`UPDATE projects SET metadata_updated_at = now() - interval '1 hour'`)
	if err := e.metadataSweep()(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, again := e.policy(); again != after {
		t.Fatalf("policy version moved without a change: %d -> %d", after, again)
	}
}

func TestAdvanceDefaultBranch(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.svc.AdvanceDefaultBranch(ctx, "no-such-project", "trunk"); err != nil {
		t.Fatalf("a project with no policy has nothing to advance: %v", err)
	}
	if err := e.svc.AdvanceDefaultBranch(ctx, "p", "bad..name"); err == nil {
		t.Fatal("invalid branch accepted")
	}
	if err := e.svc.SetPolicy(ctx, "p", 1, Policy{DefaultBranch: "main", ProtectedPatterns: []string{"release/*"}, AllowedPatterns: []string{"feature/*"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.AdvanceDefaultBranch(ctx, "p", "trunk"); err != nil {
		t.Fatal(err)
	}
	p, version := e.policy()
	if p.DefaultBranch != "trunk" || len(p.PreviousDefaults) != 1 || len(p.ProtectedPatterns) != 1 || len(p.AllowedPatterns) != 1 || version != 3 {
		t.Fatalf("owner patterns or history lost: %+v version %d", p, version)
	}
	if err := e.svc.AdvanceDefaultBranch(ctx, "p", "trunk"); err != nil {
		t.Fatal(err)
	}
	if _, again := e.policy(); again != version {
		t.Fatal("advancing to the current default bumped the version")
	}
}
