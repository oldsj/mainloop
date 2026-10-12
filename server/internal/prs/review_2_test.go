package prs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
	"github.com/oldsj/mainloop/server/internal/merge"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

func TestRereviewPRPostRaceRequiresFreshMergePreparation(t *testing.T) {
	e := newEnv(t)
	e.svc.Settle = 250 * time.Millisecond
	e.fake.Before("POST", pullsPath, func() {
		e.fake.Update("o", "n", func(r *githubtest.Repository) {
			r.Branches["feature"] = other
			r.Suites = map[string][]githubtest.CheckSuite{other: {{ID: 1, AppID: 9, Conclusion: "success"}}}
			r.Runs = map[string][]githubtest.CheckRun{other: {{ID: 1, AppID: 9, Name: "ci", Conclusion: "success"}}}
		})
	})
	e.run()
	ctx := context.Background()
	created, err := e.svc.Open(ctx, e.input("pr-race"))
	if err != nil || created.State != dbstate.PRUncertain || e.posts() != 1 || e.prs() != 1 {
		t.Fatalf("POST race: state=%s error=%v POSTs=%d PRs=%d", created.State, err, e.posts(), e.prs())
	}
	remote, found := e.fake.PullRequestOf("o", "n", 1)
	if !found || remote.HeadSHA != other {
		t.Fatalf("POST did not race the head as intended: found=%v head=%q", found, remote.HeadSHA)
	}

	svc := &merge.Service{Pool: e.pool, GitHub: e.svc.GitHub.(merge.GitHub), Log: quiet(), Settle: 5 * time.Second}
	in := merge.PrepareInput{OwnerID: owner, ProjectID: "p1", SessionID: "s1", PRNumber: 1, ExpectedSHA: tip, RequestID: "old-pin"}
	_, err = svc.Prepare(ctx, in)
	var refusal *merge.Refusal
	if !errors.As(err, &refusal) || refusal.Code != "head_moved" || e.fake.Count("PUT", "/repos/o/n/pulls/1/merge") != 0 {
		t.Fatalf("old SHA must not grant a merge: error=%v", err)
	}

	runner := reconcile.NewRunner(e.pool, quiet(), nil)
	loop := svc.Loop()
	loop.Interval = 10 * time.Millisecond
	runner.Add(loop)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = runner.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-done })
	in.ExpectedSHA, in.RequestID = other, "fresh-pin"
	merged, err := svc.Merge(ctx, in)
	if err != nil || merged.State != dbstate.MergeMerged || merged.HeadSHA != other || e.fake.Count("PUT", "/repos/o/n/pulls/1/merge") != 1 {
		t.Fatalf("fresh preparation: state=%s head=%s error=%v", merged.State, merged.HeadSHA, err)
	}
}
