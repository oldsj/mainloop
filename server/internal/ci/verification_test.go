package ci_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/ci"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
)

func TestResolve(t *testing.T) {
	usable, none := &ci.Detection{Usable: true}, &ci.Detection{}
	for _, c := range []struct {
		mode dbstate.VerificationMode
		d    *ci.Detection
		want dbstate.VerificationMode
		src  ci.Source
	}{
		{dbstate.VerifyAuto, usable, dbstate.VerifyHybrid, ci.SourceDetected},
		{dbstate.VerifyAuto, none, dbstate.VerifyLocal, ci.SourceDetected},
		{dbstate.VerifyAuto, nil, dbstate.VerifyLocal, ci.SourceDefault},
		{dbstate.VerifyCI, nil, dbstate.VerifyCI, ci.SourceOwner},
		{dbstate.VerifyLocal, usable, dbstate.VerifyLocal, ci.SourceOwner},
	} {
		if got, src := ci.Resolve(c.mode, c.d); got != c.want || src != c.src {
			t.Errorf("Resolve(%s, %+v) = %s %s, want %s %s", c.mode, c.d, got, src, c.want, c.src)
		}
	}
}

func TestVerificationDetectsCIOnTheDefaultBranchAndCachesIt(t *testing.T) {
	svc, fake := newService(t)
	ctx := context.Background()
	now := time.Now()
	svc.Now = func() time.Time { return now }
	fake.Update("o", "n", func(r *githubtest.Repository) { r.Branches = map[string]string{"main": headSHA} })

	v, err := svc.Verification(ctx, owner, "p1", false)
	if err != nil {
		t.Fatal(err)
	}
	if v.Mode != dbstate.VerifyAuto || v.Effective != dbstate.VerifyHybrid || v.Source != ci.SourceDetected || v.DetectError != "" {
		t.Fatalf("verification = %+v", v)
	}
	d := v.Detection
	if d == nil || !d.Usable || d.Branch != "main" || d.SHA != headSHA || d.Checks != 4 {
		t.Fatalf("detection = %+v", d)
	}
	// A check run with no app slug is named by itself, as a commit status is by its context.
	if want := []string{"deploy/preview", "github-actions", "image"}; !slices.Equal(d.Sources, want) {
		t.Errorf("sources = %v", d.Sources)
	}

	// A fresh detection is reused; a day later it is read again.
	reads := fake.Count("GET", "/repos/o/n/branches/main")
	if _, err := svc.Verification(ctx, owner, "p1", false); err != nil {
		t.Fatal(err)
	}
	if n := fake.Count("GET", "/repos/o/n/branches/main"); n != reads {
		t.Errorf("a fresh detection was redone")
	}
	fake.Update("o", "n", func(r *githubtest.Repository) { r.Branches["main"] = emptySHA })
	now = now.Add(ci.DetectionTTL)
	v, err = svc.Verification(ctx, owner, "p1", false)
	if err != nil || v.Effective != dbstate.VerifyLocal || v.Detection.Usable || v.Detection.SHA != emptySHA {
		t.Fatalf("after a day = %+v, %v", v, err)
	}

	// A failed refresh keeps the last detection and says why.
	fake.Fail("GET", "/repos/o/n/branches/main", 502)
	v, err = svc.Verification(ctx, owner, "p1", true)
	if err != nil || v.DetectError == "" || v.Detection == nil || v.Detection.SHA != emptySHA {
		t.Fatalf("failed refresh = %+v, %v", v, err)
	}
}

func TestVerificationModeIsTheOwnersChoice(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	v, err := svc.SetVerificationMode(ctx, owner, "p1", dbstate.VerifyCI)
	if err != nil || v.Mode != dbstate.VerifyCI || v.Effective != dbstate.VerifyCI || v.Source != ci.SourceOwner {
		t.Fatalf("set = %+v, %v", v, err)
	}
	if _, err := svc.SetVerificationMode(ctx, owner, "p1", "fast"); !errors.Is(err, ci.ErrInvalidMode) {
		t.Errorf("bad mode = %v", err)
	}
	if _, err := svc.SetVerificationMode(ctx, owner, "foreign", dbstate.VerifyLocal); !errors.Is(err, ci.ErrProjectNotFound) {
		t.Errorf("foreign = %v", err)
	}
	if _, err := svc.Verification(ctx, owner, "foreign", false); !errors.Is(err, ci.ErrProjectNotFound) {
		t.Errorf("foreign read = %v", err)
	}
}

func TestVerificationFindsCIThatRunsOnlyOnPullRequests(t *testing.T) {
	svc, fake := newService(t)
	ctx := context.Background()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fake.Update("o", "n", func(r *githubtest.Repository) {
		// Nothing reports on main. The newest closed pull request was closed unmerged (its checks
		// say nothing about the merged code path); the one before was merged and has checks.
		r.Branches = map[string]string{"main": emptySHA}
		r.PullRequests = append(r.PullRequests,
			githubtest.PullRequest{Number: 40, HeadRef: "a", HeadSHA: greenSHA, Merged: true, Closed: true, UpdatedAt: t0},
			githubtest.PullRequest{Number: 41, HeadRef: "b", HeadSHA: headSHA, Closed: true, UpdatedAt: t0.Add(time.Hour)},
		)
	})
	v, err := svc.Verification(ctx, owner, "p1", false)
	if err != nil {
		t.Fatal(err)
	}
	d := v.Detection
	if v.Effective != dbstate.VerifyHybrid || d == nil || !d.Usable || d.PR != 40 || d.SHA != greenSHA || d.Branch != "main" || d.Checks != 1 {
		t.Fatalf("verification = %+v, detection = %+v", v, d)
	}
	if n := fake.Count("GET", "/repos/o/n/commits/"+headSHA+"/check-runs"); n != 0 {
		t.Errorf("read checks on an unmerged pull request's head %d times", n)
	}
}
