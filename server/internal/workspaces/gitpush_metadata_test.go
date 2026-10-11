package workspaces

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/projects"
	"github.com/oldsj/mainloop/server/internal/pushgate"
)

// githubStub answers the project-metadata sweep with a fixed default branch.
type githubStub struct{ branch string }

func (g githubStub) Metadata(context.Context, github.Repo) (*github.Metadata, error) {
	return &github.Metadata{
		Repo: github.Repo{Owner: "oldsj", Name: "mainloop"}, HTMLURL: "https://github.com/oldsj/mainloop", DefaultBranch: g.branch,
	}, nil
}

func (githubStub) OpenPullRequests(context.Context, github.Repo, int) ([]github.PullRequest, error) {
	return nil, nil
}

func (githubStub) RecentCommits(context.Context, github.Repo, string, int) ([]github.Commit, error) {
	return nil, nil
}

func metadataSweepFor(h *harness, branch string) func(context.Context) error {
	svc := &projects.Service{
		Pool: h.svc.Pool, Remote: githubStub{branch}, OwnerID: owner,
		Policy: &pushgate.Service{Pool: h.svc.Pool}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return svc.MetadataSweep().Run
}

// The state production reaches: the project is imported with no default branch (a repo URL, or a
// GitHub blip), then a task workspace is created. The create must not decide "no push" while the
// default is unknown. It waits, the metadata sweep fills the default, the retried create reserves
// the push reference, and the workspace pushes through the gate.
func TestTaskWorkspaceCreatedBeforeProjectMetadataGainsPushAuthority(t *testing.T) {
	h := newHarness(t, nil)
	w, a, u := queuedPushWorkspaceWithDefault(t, h, "feature/test", true, "")
	f := newPushFixture(t, h, w, u)
	var def string
	if err := h.svc.Pool.QueryRow(bg, `SELECT default_branch FROM projects WHERE id=$1`, w.ProjectID).Scan(&def); err != nil || def != "" {
		t.Fatalf("the project was not imported without a default: %q %v", def, err)
	}

	createPushSession(t, h)
	op := h.op(w.ID, string(dbstate.OpCreate))
	if op.State != dbstate.OpRetrying || op.LastError == nil || !strings.Contains(*op.LastError, "default branch") {
		t.Fatalf("create did not wait for the default branch: %+v", op)
	}
	if h.row(w.ID).GitPushDecided || len(h.fake.CreateRequests()) != 0 {
		t.Fatal("create froze a push decision or created a Session before the default was known")
	}
	if _, err := h.dq.GetWorkspacePushReservation(bg, w.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("a reservation exists before the default was known")
	}

	if err := metadataSweepFor(h, "main")(bg); err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE workspace_ops SET next_attempt_at=now() WHERE workspace_id=$1`, w.ID)
	createPushSession(t, h)
	r, err := h.dq.GetWorkspacePushReservation(bg, w.ID)
	if err != nil || r.AttemptID != "attempt" || !h.row(w.ID).GitPushDecided {
		t.Fatalf("the retried create did not reserve push authority: %+v %v", r, err)
	}

	activatePushAttempt(h, w.ID)
	if err = reconcilePush(t, h, w.ID); err != nil {
		t.Fatal(err)
	}
	f.pushThroughGate(h, a, w, u)
}

// A permanently failing GitHub does not hold the create forever: it gives up at the retry window
// with the reason recorded, and no push decision is frozen on the way.
func TestTaskWorkspaceWaitForProjectDefaultIsBounded(t *testing.T) {
	h := newHarness(t, nil)
	w, _, _ := queuedPushWorkspaceWithDefault(t, h, "feature/test", true, "")
	createPushSession(t, h)
	h.exec(`UPDATE workspace_ops SET first_failed_at=now()-interval '1 hour', next_attempt_at=now() WHERE workspace_id=$1`, w.ID)
	createPushSession(t, h)
	op := h.op(w.ID, string(dbstate.OpCreate))
	if op.State != dbstate.OpGaveUp || op.LastError == nil || !strings.Contains(*op.LastError, "default branch") {
		t.Fatalf("the wait was not bounded or did not say why: %+v", op)
	}
	if h.row(w.ID).GitPushDecided {
		t.Fatal("a push decision was frozen")
	}
}

// Only a workspace that could get a reference waits: a standalone workspace on a project with no
// default yet is created at once, read-only, as before.
func TestStandaloneWorkspaceDoesNotWaitForProjectDefault(t *testing.T) {
	h := newHarness(t, nil)
	w, _, _ := queuedPushWorkspaceWithDefault(t, h, "feature/test", false, "")
	createPushSession(t, h)
	if !h.row(w.ID).GitPushDecided || len(h.fake.CreateRequests()) != 1 || len(h.fake.CreateRequests()[0].Credentials) != 2 {
		t.Fatal("a standalone workspace waited or got a push credential")
	}
}
