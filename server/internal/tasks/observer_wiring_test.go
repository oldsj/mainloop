package tasks_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/app"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
)

func TestProductionObserverWiringProjectsAndPublishes(t *testing.T) {
	r := newRig(t)
	id := r.projected()
	f := githubtest.New(t)
	sha := strings.Repeat("a", 40)
	f.AddRepo(githubtest.Repository{
		Owner: "o", Name: "p1", ID: 1,
		PullRequests: []githubtest.PullRequest{{Number: 7, HeadRef: "feature", HeadSHA: sha}},
		Runs:         map[string][]githubtest.CheckRun{sha: {{ID: 1, AppID: 2, Name: "test", Conclusion: "success"}}},
	})
	gh, err := github.NewApp(github.Options{AppID: fmt.Sprint(githubtest.AppID), PrivateKey: githubtest.PrivateKey(), BaseURL: f.URL})
	if err != nil {
		t.Fatal(err)
	}
	d := app.Deps{Config: mustConfig(t), Log: slog.Default(), Pool: r.pool, GitHub: gh}
	if disconnected := app.New(d).TasksAPI.Svc.Observer; disconnected != nil {
		t.Fatal("missing observer was wired as a typed nil")
	}
	d.TaskObserver = &github.TaskObserver{App: gh}
	r.svc = app.New(d).TasksAPI.Svc
	if r.svc.Observer == nil {
		t.Fatal("configured observer is missing")
	}
	before := r.updates(id)
	r.sweep(r.svc.ProjectionSweep())
	got := r.view(r.own, id)
	if got.Projection.PrNumber == nil || *got.Projection.PrNumber != 7 ||
		string(got.Projection.CiState) != "success" || got.Projection.CiHeadSha == nil || *got.Projection.CiHeadSha != sha ||
		r.updates(id) != before+1 || got.Task.Status != "running" {
		t.Fatalf("production projection = %+v, events %d", got, r.updates(id))
	}
	// Reads use the stored observation and make no GitHub requests.
	calls := len(f.Requests())
	if _, err := r.svc.Get(context.Background(), r.own, id); err != nil {
		t.Fatal(err)
	}
	if len(f.Requests()) != calls {
		t.Fatal("task read made a GitHub request")
	}
}
