package github_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
)

func TestGetIssue(t *testing.T) {
	app, _ := pullApp(t, func(r *githubtest.Repository) {
		r.IssueList = []githubtest.Issue{
			{Number: 5, Title: "Bug", Body: "It breaks", State: "open", Labels: []string{"bug", "p1"}},
			{Number: 6, Title: "A PR", IsPullRequest: true},
		}
	})
	ctx := context.Background()

	got, err := app.GetIssue(ctx, repoON, 5)
	if err != nil || got.Number != 5 || got.Title != "Bug" || got.Body != "It breaks" || !got.BodyAvailable ||
		got.State != "open" || len(got.Labels) != 2 || got.Labels[0] != "bug" || got.Labels[1] != "p1" {
		t.Fatalf("issue = %+v, %v", got, err)
	}

	if _, err := app.GetIssue(ctx, repoON, 99); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("missing issue = %v", err)
	}

	// A pull request's number answers not found: this reader serves issues only.
	if _, err := app.GetIssue(ctx, repoON, 6); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("pull request masquerading as an issue = %v", err)
	}
}

func TestListDependabotAlerts(t *testing.T) {
	app, _ := pullApp(t, func(r *githubtest.Repository) {
		r.DependabotAlerts = []githubtest.DependabotAlert{
			{
				Number: 1, PackageName: "lodash", Ecosystem: "npm", ManifestPath: "package.json",
				Severity: "high", VulnerableVersionRange: "< 4.17.21", PatchedVersion: "4.17.21",
				GHSAID: "GHSA-xxxx", Summary: "Prototype pollution",
			},
			{Number: 2, PackageName: "old-dep", State: "fixed"},
		}
	})
	ctx := context.Background()

	got, err := app.ListDependabotAlerts(ctx, repoON)
	if err != nil || len(got) != 1 || got[0].Number != 1 || got[0].PackageName != "lodash" ||
		got[0].Ecosystem != "npm" || got[0].Severity != "high" || got[0].VulnerableVersionRange != "< 4.17.21" ||
		got[0].PatchedVersion != "4.17.21" || got[0].AdvisoryGHSAID != "GHSA-xxxx" || got[0].AdvisorySummary != "Prototype pollution" {
		t.Fatalf("alerts = %+v, %v", got, err)
	}
}

func TestListDependabotAlertsEmptyIsNotAnError(t *testing.T) {
	app, _ := pullApp(t, nil)
	got, err := app.ListDependabotAlerts(context.Background(), repoON)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty alerts = %+v, %v", got, err)
	}
}

func TestIssueWritesAndLookups(t *testing.T) {
	app, fake := pullApp(t, func(r *githubtest.Repository) {
		r.Labels = []string{"bug", "p1"}
		r.IssueList = []githubtest.Issue{
			{Number: 5, Title: "Old", CreatedAt: time.Now().Add(-time.Hour).UTC()},
			{Number: 6, Title: "A PR", IsPullRequest: true, CreatedAt: time.Now().UTC()},
		}
	})
	ctx := context.Background()
	start := time.Now().Add(-time.Second)

	labels, err := app.LabelNames(ctx, repoON)
	if err != nil || len(labels) != 2 || labels[0] != "bug" {
		t.Fatalf("labels = %v, %v", labels, err)
	}
	made, err := app.CreateIssue(ctx, repoON, "New", "body text", []string{"bug"})
	if err != nil || made.Number <= 6 || made.HTMLURL == "" || made.UserType != "Bot" || made.Body != "body text" {
		t.Fatalf("created = %+v, %v", made, err)
	}
	if _, err := app.CreateIssue(ctx, repoON, "New", "", []string{"missing"}); err == nil {
		t.Error("an unknown label was accepted")
	}

	// The listing since start has the new issue and stops before the old one; pull requests are
	// left out.
	since, err := app.IssuesCreatedSince(ctx, repoON, start)
	if err != nil || len(since) != 1 || since[0].Number != made.Number {
		t.Fatalf("since = %+v, %v", since, err)
	}

	c, err := app.CommentIssue(ctx, repoON, 5, "a finding")
	if err != nil || c.CommentID == 0 || c.Number != 5 || c.HTMLURL == "" {
		t.Fatalf("comment = %+v, %v", c, err)
	}
	got, err := app.IssueCommentsSince(ctx, repoON, 5, start)
	if err != nil || len(got) != 1 || got[0].CommentID != c.CommentID || got[0].Body != "a finding" || got[0].UserType != "Bot" {
		t.Fatalf("comments = %+v, %v", got, err)
	}
	if _, err := app.IssueCommentsSince(ctx, repoON, 99, start); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("comments on a missing issue = %v", err)
	}
	// Each call asked for the least it needed: Issues write to write, Issues read to read.
	for _, m := range fake.Mints() {
		if m.Permissions["pull_requests"] != "" || m.Permissions["contents"] != "" {
			t.Errorf("mint asked for %v", m.Permissions)
		}
	}
}
