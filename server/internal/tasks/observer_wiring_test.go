package tasks_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

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

func TestReviewCollectionTimeSurvivesPersistenceDelay(t *testing.T) {
	for _, path := range []string{"changed", "unchanged"} {
		t.Run(path, func(t *testing.T) {
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
			observer := &github.TaskObserver{App: gh}
			r.svc = app.New(app.Deps{
				Config: mustConfig(t), Log: slog.Default(), Pool: r.pool, TaskObserver: observer,
			}).TasksAPI.Svc
			if path == "unchanged" {
				r.project()
				if got := r.view(r.own, id).Projection; string(got.CiState) != "success" {
					t.Fatalf("initial projection = %+v", got)
				}
			}
			before, events := r.view(r.own, id), r.updates(id)
			// Model a one-minute collection followed by six minutes before persistence.
			// Only the observer's clock is injected; the DB and task read use their real clocks.
			finished := time.Now().Add(-6 * time.Minute).UTC().Truncate(time.Microsecond)
			clockCalls := 0
			observer.Now = func() time.Time {
				clockCalls++
				if clockCalls == 1 {
					return finished.Add(-time.Minute)
				}
				return finished
			}
			r.project()
			got := r.view(r.own, id)
			if clockCalls != 2 || got.Projection.PrNumber == nil || *got.Projection.PrNumber != 7 ||
				got.Projection.CiHeadSha == nil || *got.Projection.CiHeadSha != sha {
				t.Fatalf("valid one-minute collection was lost: %+v, clock calls %d", got.Projection, clockCalls)
			}
			if string(got.Projection.CiState) != "unknown" {
				t.Fatalf("six-minute-old observation was restamped fresh: %+v", got.Projection)
			}
			if got.Projection.ObservedAt == nil || !got.Projection.ObservedAt.Equal(finished) {
				t.Fatalf("observation time = %v, want collection finish %v", got.Projection.ObservedAt, finished)
			}
			if path == "unchanged" {
				if got.Task.Version != before.Task.Version || r.updates(id) != events {
					t.Fatal("unchanged observation changed the task version or published an event")
				}
			} else if got.Task.Version != before.Task.Version+1 || r.updates(id) != events+1 {
				t.Fatal("changed observation did not update the task and publish exactly one event")
			}
		})
	}
}
