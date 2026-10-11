package github_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
)

func greenObservation(t *testing.T) (*github.TaskObserver, *githubtest.Server) {
	t.Helper()
	a, f := pullApp(t, func(r *githubtest.Repository) {
		r.Runs = map[string][]githubtest.CheckRun{
			tipSHA: {{ID: 1, AppID: 2, Name: "test", Conclusion: "success"}},
		}
	})
	return &github.TaskObserver{App: a}, f
}

func TestTaskObserverPRStatesAndReadScope(t *testing.T) {
	for _, state := range []string{"open", "closed", "merged"} {
		t.Run(state, func(t *testing.T) {
			o, f := greenObservation(t)
			f.Update("o", "n", func(r *githubtest.Repository) {
				r.PullRequests[0].Closed = state != "open"
				r.PullRequests[0].Merged = state == "merged"
				// A non-default base must not hide the task PR.
				r.PullRequests[0].BaseRef = "release"
			})
			got, err := o.Observe(context.Background(), "O/N", "feature")
			if err != nil || got.PRNumber != 1 || got.PRState != state || got.CIState != "success" ||
				got.PRHeadSHA != tipSHA || got.CIHeadSHA != tipSHA || got.PRHeadRef != "feature" ||
				got.PRRepo != "O/N" || got.PRURL != "https://github.com/O/N/pull/1" {
				t.Fatalf("observation = %+v, %v", got, err)
			}
			if f.Count("GET", "/repos/O/N/pulls/1") != 2 {
				t.Fatal("PR was not re-read after CI")
			}
			for _, req := range f.Requests() {
				if strings.HasPrefix(req.Path, "/repos/") && req.Method != http.MethodGet {
					t.Fatalf("repository write: %+v", req)
				}
			}
			for _, mint := range f.Mints() {
				for name, level := range mint.Permissions {
					if level != "read" || (name != "pull_requests" && name != "checks" && name != "statuses") {
						t.Fatalf("excess permission: %v", mint.Permissions)
					}
				}
			}
		})
	}
}

func TestTaskObserverSelectsOpenThenNewestPR(t *testing.T) {
	for _, open := range []bool{true, false} {
		t.Run(fmt.Sprint(open), func(t *testing.T) {
			o, f := greenObservation(t)
			f.Update("o", "n", func(r *githubtest.Repository) {
				r.PullRequests = []githubtest.PullRequest{
					{Number: 9, HeadRef: "feature", HeadSHA: tipSHA, Closed: true, Merged: true},
					{Number: 3, HeadRef: "feature", HeadSHA: tipSHA, Closed: !open},
					{Number: 99, HeadRef: "other", HeadSHA: tipSHA},
				}
			})
			got, err := o.Observe(context.Background(), "o/n", "feature")
			want := 9
			if open {
				want = 3
			}
			if err != nil || got.PRNumber != want {
				t.Fatalf("observation = %+v, %v; want PR %d", got, err, want)
			}
		})
	}
}

func TestTaskObserverCIStates(t *testing.T) {
	cases := []struct {
		name, want string
		runs       []githubtest.CheckRun
		statuses   []githubtest.Status
	}{
		{"no_results", "unknown", nil, nil},
		{"run_success", "success", []githubtest.CheckRun{{ID: 1, AppID: 2, Name: "ci", Conclusion: "success"}}, nil},
		{"run_pending", "pending", []githubtest.CheckRun{{ID: 1, AppID: 2, Name: "ci", Status: "in_progress"}}, nil},
		{"run_failed", "failure", []githubtest.CheckRun{{ID: 1, AppID: 2, Name: "ci", Conclusion: "failure"}}, nil},
		{"run_neutral", "failure", []githubtest.CheckRun{{ID: 1, AppID: 2, Name: "ci", Conclusion: "neutral"}}, nil},
		{"run_skipped", "failure", []githubtest.CheckRun{{ID: 1, AppID: 2, Name: "ci", Conclusion: "skipped"}}, nil},
		{"run_unknown", "failure", []githubtest.CheckRun{{ID: 1, AppID: 2, Name: "ci", Status: "new_state"}}, nil},
		{"status_success", "success", nil, []githubtest.Status{{ID: 1, Context: "ci", State: "success"}}},
		{"status_pending", "pending", nil, []githubtest.Status{{ID: 1, Context: "ci", State: "pending"}}},
		{"status_failure", "failure", nil, []githubtest.Status{{ID: 1, Context: "ci", State: "failure"}}},
		{"status_error", "failure", nil, []githubtest.Status{{ID: 1, Context: "ci", State: "error"}}},
		{"status_unknown", "failure", nil, []githubtest.Status{{ID: 1, Context: "ci", State: "new_state"}}},
		{"both_success", "success", []githubtest.CheckRun{{ID: 1, AppID: 2, Name: "ci", Conclusion: "success"}}, []githubtest.Status{{ID: 1, Context: "lint", State: "success"}}},
		{"status_pending_over_run_success", "pending", []githubtest.CheckRun{{ID: 1, AppID: 2, Name: "ci", Conclusion: "success"}}, []githubtest.Status{{ID: 1, Context: "lint", State: "pending"}}},
		{"run_pending_over_status_success", "pending", []githubtest.CheckRun{{ID: 1, AppID: 2, Name: "ci", Status: "queued"}}, []githubtest.Status{{ID: 1, Context: "lint", State: "success"}}},
		{"failure_over_pending", "failure", []githubtest.CheckRun{{ID: 1, AppID: 2, Name: "ci", Status: "waiting"}}, []githubtest.Status{{ID: 1, Context: "lint", State: "failure"}}},
		{"latest_run_wins", "success", []githubtest.CheckRun{{ID: 2, AppID: 2, Name: "ci", Conclusion: "success"}, {ID: 1, AppID: 2, Name: "ci", Conclusion: "failure"}}, nil},
		{"app_identity_is_separate", "failure", []githubtest.CheckRun{{ID: 2, AppID: 2, Name: "ci", Conclusion: "success"}, {ID: 1, AppID: 3, Name: "ci", Conclusion: "failure"}}, nil},
		{"latest_status_wins", "success", nil, []githubtest.Status{{ID: 2, Context: "ci", State: "success"}, {ID: 1, Context: "ci", State: "failure"}}},
		{"status_time_precedes_id", "failure", nil, []githubtest.Status{{ID: 1, Context: "ci", State: "failure", CreatedAt: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)}, {ID: 2, Context: "ci", State: "success", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}},
		{"status_id_breaks_tie", "failure", nil, []githubtest.Status{{ID: 1, Context: "ci", State: "success", CreatedAt: time.Unix(1, 0)}, {ID: 2, Context: "ci", State: "failure", CreatedAt: time.Unix(1, 0)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, f := greenObservation(t)
			f.Update("o", "n", func(r *githubtest.Repository) {
				r.Runs[tipSHA] = tc.runs
				r.Statuses = map[string][]githubtest.Status{tipSHA: tc.statuses}
			})
			got, err := o.Observe(context.Background(), "o/n", "feature")
			if err != nil || got.CIState != tc.want || got.PRNumber != 1 {
				t.Fatalf("observation = %+v, %v; want CI %s", got, err, tc.want)
			}
			if tc.want == "unknown" && got.CIHeadSHA != "" {
				t.Fatal("empty CI was attributed to a head")
			}
		})
	}
}

func TestTaskObserverRejectsMismatchedHeadsAndIdentity(t *testing.T) {
	for _, scenario := range []string{"check_head", "head_during_checks", "branch_during_checks", "closed_during_checks", "missing_repo", "branch_before_detail"} {
		t.Run(scenario, func(t *testing.T) {
			o, f := greenObservation(t)
			switch scenario {
			case "check_head":
				f.Update("o", "n", func(r *githubtest.Repository) { r.Runs[tipSHA][0].HeadSHA = strings.Repeat("b", 40) })
			case "missing_repo":
				f.Update("o", "n", func(r *githubtest.Repository) { r.PullRequests[0].HeadRepoGone = true })
			case "branch_before_detail":
				f.Before("GET", "/repos/o/n/pulls/1", func() {
					f.Update("o", "n", func(r *githubtest.Repository) { r.PullRequests[0].HeadRef = "other" })
				})
			default:
				f.Before("GET", "/repos/o/n/commits/"+tipSHA+"/check-runs", func() {
					f.Update("o", "n", func(r *githubtest.Repository) {
						switch scenario {
						case "head_during_checks":
							r.PullRequests[0].HeadSHA = strings.Repeat("b", 40)
						case "branch_during_checks":
							r.PullRequests[0].HeadRef = "other"
						case "closed_during_checks":
							r.PullRequests[0].Closed = true
						}
					})
				})
			}
			got, err := o.Observe(context.Background(), "o/n", "feature")
			if got != (github.TaskObservation{}) || (scenario != "check_head" && err != nil) {
				t.Fatalf("untrusted head was observed: %+v, %v", got, err)
			}
			if scenario == "check_head" && !errors.Is(err, github.ErrObservationRetry) {
				t.Fatalf("mismatched check was not unavailable: %v", err)
			}
		})
	}
}

func TestTaskObserverReadsAllPages(t *testing.T) {
	for _, listing := range []string{"pulls", "check-runs", "statuses"} {
		t.Run(listing, func(t *testing.T) {
			o, f := greenObservation(t)
			path := "/repos/o/n/commits/" + tipSHA + "/" + listing
			wantNumber, wantCI := 1, "failure"
			if listing == "pulls" {
				path, wantNumber, wantCI = "/repos/o/n/pulls", 101, "success"
			}
			f.Update("o", "n", func(r *githubtest.Repository) {
				for i := 1; i <= 101; i++ {
					switch listing {
					case "pulls":
						r.PullRequests = append(r.PullRequests[:i-1], githubtest.PullRequest{Number: i, HeadRef: "feature", HeadSHA: tipSHA})
					case "check-runs":
						conclusion := "success"
						if i == 101 {
							conclusion = "failure"
						}
						r.Runs[tipSHA] = append(r.Runs[tipSHA][:i-1], githubtest.CheckRun{ID: int64(i), AppID: 2, Name: fmt.Sprint(i), Conclusion: conclusion})
					case "statuses":
						if r.Statuses == nil {
							r.Statuses = map[string][]githubtest.Status{}
						}
						state := "success"
						if i == 101 {
							state = "failure"
						}
						r.Statuses[tipSHA] = append(r.Statuses[tipSHA], githubtest.Status{ID: int64(i), Context: fmt.Sprint(i), State: state})
					}
				}
			})
			got, err := o.Observe(context.Background(), "o/n", "feature")
			if err != nil || got.PRNumber != wantNumber || got.CIState != wantCI || f.Count("GET", path) != 2 {
				t.Fatalf("observation = %+v, %v; %d pages", got, err, f.Count("GET", path))
			}
		})
	}
}

func TestTaskObserverStaleObservationAndNoCache(t *testing.T) {
	for _, elapsed := range []time.Duration{5 * time.Minute, 5*time.Minute + time.Nanosecond, -time.Second} {
		t.Run(elapsed.String(), func(t *testing.T) {
			o, _ := greenObservation(t)
			at := time.Now()
			calls := 0
			o.Now = func() time.Time {
				calls++
				if calls == 1 {
					return at
				}
				return at.Add(elapsed)
			}
			got, err := o.Observe(context.Background(), "o/n", "feature")
			if elapsed == 5*time.Minute {
				if err != nil || got.CIState != "success" {
					t.Fatalf("boundary = %+v, %v", got, err)
				}
			} else if got != (github.TaskObservation{}) || !errors.Is(err, github.ErrObservationStale) || !errors.Is(err, github.ErrObservationRetry) {
				t.Fatalf("stale observation = %+v, %v", got, err)
			}
		})
	}
	o, f := greenObservation(t)
	if got, err := o.Observe(context.Background(), "o/n", "feature"); err != nil || got.CIState != "success" {
		t.Fatalf("initial = %+v, %v", got, err)
	}
	f.Update("o", "n", func(r *githubtest.Repository) { r.Runs[tipSHA][0].Conclusion = "failure" })
	if got, err := o.Observe(context.Background(), "o/n", "feature"); err != nil || got.CIState != "failure" {
		t.Fatalf("fresh read reused a cached pass: %+v, %v", got, err)
	}
	f.Fail("GET", "/repos/o/n/pulls", http.StatusServiceUnavailable)
	if got, err := o.Observe(context.Background(), "o/n", "feature"); got != (github.TaskObservation{}) || !errors.Is(err, github.ErrObservationRetry) {
		t.Fatalf("failed read reused cached CI: %+v, %v", got, err)
	}
}

func TestTaskObserverErrorClasses(t *testing.T) {
	paths := []string{
		"/repos/o/n/installation", "/repos/o/n/pulls", "/repos/o/n/pulls/1",
		"/repos/o/n/commits/" + tipSHA + "/check-runs", "/repos/o/n/commits/" + tipSHA + "/statuses",
	}
	for _, path := range paths {
		for _, status := range []int{401, 403, 404, 422, 429, 500, 502, 503} {
			t.Run(path+fmt.Sprint(status), func(t *testing.T) {
				o, f := greenObservation(t)
				f.Fail("GET", path, status)
				got, err := o.Observe(context.Background(), "o/n", "feature")
				if status == 404 {
					if err != nil || got.CIState == "success" {
						t.Fatalf("404 = %+v, %v", got, err)
					}
					if strings.Contains(path, "/commits/") {
						if got.PRNumber != 1 || got.CIState != "unknown" {
							t.Fatalf("absent CI lost the PR: %+v", got)
						}
					} else if got != (github.TaskObservation{}) {
						t.Fatalf("absent PR = %+v", got)
					}
					return
				}
				want := github.ErrObservationRefused
				if status == 429 || status >= 500 {
					want = github.ErrObservationRetry
				}
				if got != (github.TaskObservation{}) || !errors.Is(err, want) || strings.Contains(err.Error(), githubtest.SecretMarker) {
					t.Fatalf("%d = %+v, %v; want %v", status, got, err, want)
				}
			})
		}
		t.Run(path+"_rate_limit_403", func(t *testing.T) {
			o, f := greenObservation(t)
			f.RateLimit("GET", path)
			got, err := o.Observe(context.Background(), "o/n", "feature")
			if got != (github.TaskObservation{}) || !errors.Is(err, github.ErrObservationRetry) {
				t.Fatalf("rate limit = %+v, %v", got, err)
			}
		})
	}
}

func TestTaskObserverMissingPRAndInvalidInputs(t *testing.T) {
	o, f := greenObservation(t)
	if got, err := o.Observe(context.Background(), "o/n", "missing"); err != nil || got != (github.TaskObservation{}) {
		t.Fatalf("absent branch PR = %+v, %v", got, err)
	}
	before := len(f.Requests())
	for _, input := range [][2]string{{"invalid", "feature"}, {"o/n", ""}, {"o/n", "x/../feature"}} {
		if got, err := o.Observe(context.Background(), input[0], input[1]); got != (github.TaskObservation{}) || !errors.Is(err, github.ErrObservationRefused) {
			t.Fatalf("invalid input = %+v, %v", got, err)
		}
	}
	if len(f.Requests()) != before {
		t.Fatal("invalid input sent a request")
	}
	var disconnected *github.TaskObserver
	if _, err := disconnected.Observe(context.Background(), "o/n", "feature"); !errors.Is(err, github.ErrObservationRetry) {
		t.Fatalf("nil observer = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := o.Observe(ctx, "o/n", "feature"); got != (github.TaskObservation{}) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled = %+v, %v", got, err)
	}
}

func TestTaskObserverTokenMintErrorClasses(t *testing.T) {
	for _, status := range []int{401, 403, 404, 429, 503, 0} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			o, f := greenObservation(t)
			if _, err := o.Observe(context.Background(), "o/n", "feature"); err != nil {
				t.Fatal(err)
			}
			path := fmt.Sprintf("/app/installations/%d/access_tokens", f.Mints()[0].InstallationID)
			if status == 0 {
				f.RateLimit("POST", path)
			} else {
				f.Fail("POST", path, status)
			}
			// A fresh App has no cached installation token to hide the refused mint.
			a, err := github.NewApp(github.Options{AppID: fmt.Sprint(githubtest.AppID), PrivateKey: githubtest.PrivateKey(), BaseURL: f.URL})
			if err != nil {
				t.Fatal(err)
			}
			o.App = a
			got, err := o.Observe(context.Background(), "o/n", "feature")
			if status == 404 {
				if got != (github.TaskObservation{}) || err != nil {
					t.Fatalf("missing installation = %+v, %v", got, err)
				}
				return
			}
			want := github.ErrObservationRefused
			if status == 0 || status == 429 || status >= 500 {
				want = github.ErrObservationRetry
			}
			if got != (github.TaskObservation{}) || !errors.Is(err, want) {
				t.Fatalf("mint failure = %+v, %v; want %v", got, err, want)
			}
		})
	}
}

func TestTaskObserverLostReadReplyAndDisappearingPR(t *testing.T) {
	o, f := greenObservation(t)
	f.LoseReply("GET", "/repos/o/n/pulls/1", 3)
	if got, err := o.Observe(context.Background(), "o/n", "feature"); got != (github.TaskObservation{}) || !errors.Is(err, github.ErrObservationRetry) {
		t.Fatalf("lost read reply = %+v, %v", got, err)
	}
	o, f = greenObservation(t)
	f.Before("GET", "/repos/o/n/commits/"+tipSHA+"/check-runs", func() {
		f.Fail("GET", "/repos/o/n/pulls/1", http.StatusNotFound)
	})
	if got, err := o.Observe(context.Background(), "o/n", "feature"); got != (github.TaskObservation{}) || err != nil {
		t.Fatalf("disappeared PR = %+v, %v", got, err)
	}
}

func TestTaskObserverRejectsPartialPages(t *testing.T) {
	o, f := greenObservation(t)
	f.Update("o", "n", func(r *githubtest.Repository) {
		r.Runs[tipSHA] = nil
		for i := 1; i <= 1001; i++ {
			r.Runs[tipSHA] = append(r.Runs[tipSHA], githubtest.CheckRun{ID: int64(i), AppID: 2, Name: fmt.Sprint(i), Conclusion: "success"})
		}
	})
	if got, err := o.Observe(context.Background(), "o/n", "feature"); got != (github.TaskObservation{}) || !errors.Is(err, github.ErrObservationRetry) {
		t.Fatalf("over-cap listing = %+v, %v", got, err)
	}
	// A failure on a later page must discard all the earlier successful results.
	o, f = greenObservation(t)
	f.Update("o", "n", func(r *githubtest.Repository) {
		r.Statuses = map[string][]githubtest.Status{}
		for i := 1; i <= 201; i++ {
			r.Statuses[tipSHA] = append(r.Statuses[tipSHA], githubtest.Status{ID: int64(i), Context: fmt.Sprint(i), State: "success"})
		}
	})
	path := "/repos/o/n/commits/" + tipSHA + "/statuses"
	f.Before("GET", path, func() {
		f.Before("GET", path, func() { f.Fail("GET", path, http.StatusServiceUnavailable) })
	})
	got, err := o.Observe(context.Background(), "o/n", "feature")
	if got != (github.TaskObservation{}) || !errors.Is(err, github.ErrObservationRetry) {
		t.Fatalf("partial pages = %+v, %v", got, err)
	}
	var pages []string
	for _, req := range f.Requests() {
		if req.Path == path {
			q, parseErr := url.ParseQuery(req.Query)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			pages = append(pages, q.Get("page"))
		}
	}
	// githubtest snapshots Fail before its Before hook, so page 3 receives the refusal.
	if strings.Join(pages, ",") != "1,2,3" {
		t.Fatalf("unexpected pages: %v", pages)
	}
}
