package merge_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
	"github.com/oldsj/mainloop/server/internal/merge"
)

// These tests re-create the G10 review probes for the merge evidence (findings 1, 7, 8, 9, 11)
// and port the Python cases they cite. None of them may send the PUT.

// prepared reads the proposal for the one pull request and returns it.
func (e *env) prepared(req string) merge.View {
	e.t.Helper()
	v, err := e.svc.Prepare(context.Background(), e.input(req))
	if err != nil {
		e.t.Fatalf("prepare = %v", err)
	}
	return v
}

// unavailable requires the proposal to take the approval route for the given reason.
func (e *env) unavailable(req, reason string) {
	e.t.Helper()
	v := e.prepared(req)
	if v.Route != dbstate.RouteApproval || !slices.Contains(v.Summary.UnavailableReasons, reason) || e.puts() != 0 {
		e.t.Fatalf("want unavailable %q: route %s reasons %v puts %d", reason, v.Route, v.Summary.UnavailableReasons, e.puts())
	}
}

// refused requires Prepare to be refused with the code.
func (e *env) refused(req, code string) {
	e.t.Helper()
	_, err := e.svc.Prepare(context.Background(), e.input(req))
	var ref *merge.Refusal
	if !errors.As(err, &ref) || ref.Code != code || e.puts() != 0 {
		e.t.Fatalf("want refusal %q: err %v puts %d", code, err, e.puts())
	}
}

// never requires the loop to hold or end the merge without sending the PUT.
func (e *env) never() merge.View {
	e.t.Helper()
	e.run()
	e.svc.Settle = 200 * time.Millisecond
	v, err := e.svc.Merge(context.Background(), e.input("r1"))
	if err != nil {
		var ref *merge.Refusal
		if !errors.As(err, &ref) {
			e.t.Fatalf("merge = %v", err)
		}
	}
	if v.State == dbstate.MergeMerged || e.puts() != 0 {
		e.t.Fatalf("merged: view %+v, puts %d", v, e.puts())
	}
	return v
}

const baseTip = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// movedMain makes the default branch one commit ahead of the merge base, with the given files
// changed on it, and the head's own files as the comparison against it.
func (e *env) movedMain(baseFiles, headFiles []githubtest.File) {
	e.t.Helper()
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		mb := githubtest.TipOf(r, r.DefaultBranch)
		r.Branches[r.DefaultBranch] = baseTip
		r.PullRequests[0].Files = headFiles
		r.Compares = map[string]githubtest.Compare{
			baseTip + "..." + tip: {
				Status: "diverged", AheadBy: 1, BehindBy: 1, Base: baseTip, MergeBase: mb,
				Commits: []githubtest.CompareCommit{{SHA: tip, Parents: []string{mb}}}, Files: headFiles,
			},
			mb + "..." + baseTip: {
				Status: "ahead", AheadBy: 1, Base: mb, MergeBase: mb,
				Commits: []githubtest.CompareCommit{{SHA: baseTip, Parents: []string{mb}}}, Files: baseFiles,
			},
		}
	})
}

func TestReviewHeadSideFilesCannotProveProtectedMergePaths(t *testing.T) {
	// main renamed src/build.yml into the workflows directory; the head only edits the old path.
	e := newEnv(t)
	e.movedMain(
		[]githubtest.File{{Filename: ".github/workflows/build.yml", Status: "renamed", PreviousFilename: "src/build.yml"}},
		[]githubtest.File{{Filename: "src/build.yml", Status: "modified", Additions: 1}},
	)
	e.unavailable("r1", merge.ReasonInventoryMoved)
	if v := e.never(); v.Route != dbstate.RouteApproval {
		t.Fatalf("view = %+v", v)
	}
}

func TestBaseSideRenameOfAHeadPathIsRefused(t *testing.T) {
	// Ported from test_base_side_rename_of_a_head_path_is_refused. Compare may pair a rename that
	// Git's merge detects as a delete and an add, so both shapes are refused.
	head := []githubtest.File{{Filename: "src/build.yml", Status: "modified"}}
	for name, base := range map[string]githubtest.File{
		"renamed": {Filename: ".github/workflows/build.yml", Status: "renamed", PreviousFilename: "src/build.yml"},
		"removed": {Filename: "src/build.yml", Status: "removed"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.movedMain([]githubtest.File{base}, head)
			e.unavailable("r1", merge.ReasonInventoryMoved)
		})
	}
}

func TestBaseSideDirectoryRenameIsRefused(t *testing.T) {
	// Ported from test_base_side_directory_rename_is_refused: Git's directory rename detection
	// carries a file the head adds under a renamed-away directory into main's new directory.
	renames := []githubtest.File{
		{Filename: ".github/workflows/build.yml", Status: "renamed", PreviousFilename: "ci/build.yml"},
		{Filename: ".github/workflows/deep/test.yml", Status: "renamed", PreviousFilename: "ci/deep/test.yml"},
	}
	for _, added := range []string{"ci/new.yml", "ci/deep/new.yml", "ci/fresh/new.yml"} {
		t.Run(added, func(t *testing.T) {
			e := newEnv(t)
			e.movedMain(renames, []githubtest.File{{Filename: added, Status: "added"}})
			e.unavailable("r1", merge.ReasonInventoryMoved)
		})
	}
	t.Run("renamed head file", func(t *testing.T) {
		e := newEnv(t)
		e.movedMain(renames, []githubtest.File{{Filename: "ci/b.yml", Status: "renamed", PreviousFilename: "b.yml"}})
		e.unavailable("r1", merge.ReasonInventoryMoved)
	})
}

func TestDirectoryStillPresentOnMainIsNotRenamed(t *testing.T) {
	// Ported from test_directory_still_present_on_main_is_not_renamed.
	rename := githubtest.File{Filename: ".github/workflows/build.yml", Status: "renamed", PreviousFilename: "ci/build.yml"}
	for name, c := range map[string]struct{ base, head []githubtest.File }{
		"head changed a file main did not": {nil, []githubtest.File{{Filename: "ci/keep.yml", Status: "modified"}}},
		"main added a file in the dir":     {[]githubtest.File{{Filename: "ci/other.yml", Status: "added"}}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.movedMain(append([]githubtest.File{rename}, c.base...),
				append([]githubtest.File{{Filename: "ci/new.yml", Status: "added"}}, c.head...))
			v := e.prepared("r1")
			if v.Route != dbstate.RouteAuto || len(v.Summary.UnavailableReasons) != 0 {
				t.Fatalf("view = %+v", v)
			}
		})
	}
}

func TestMovedMainWithAnUnrelatedChangeKeepsTheAutomaticRoute(t *testing.T) {
	e := newEnv(t)
	e.movedMain(
		[]githubtest.File{{Filename: "docs/readme.md", Status: "modified"}},
		[]githubtest.File{{Filename: "server/a.go", Status: "modified"}},
	)
	v := e.prepared("r1")
	if v.Route != dbstate.RouteAuto || len(v.Summary.UnavailableReasons) != 0 || v.Summary.MergeFileCount == nil || *v.Summary.MergeFileCount != 1 {
		t.Fatalf("view = %+v", v)
	}
}

func TestMergeInventoryRefusesAMergeCommitOnMain(t *testing.T) {
	e := newEnv(t)
	e.movedMain(nil, []githubtest.File{{Filename: "server/a.go", Status: "modified"}})
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		key := githubtest.TipOf(r, "feature")
		_ = key
		for k, c := range r.Compares {
			if c.Status == "ahead" {
				c.Commits = []githubtest.CompareCommit{{SHA: baseTip, Parents: []string{c.Base, "cccccccccccccccccccccccccccccccccccccccc"}}}
				r.Compares[k] = c
			}
		}
	})
	e.unavailable("r1", merge.ReasonInventoryMerges)
}

func TestReviewUnsupportedRulesAndAppBoundChecksMustBlock(t *testing.T) {
	classic := func(inner string) string {
		return `{"required_status_checks":` + inner + `,"enforce_admins":{"enabled":false}}`
	}
	t.Run("classic check bound to another app holds", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Update("o", "n", func(r *githubtest.Repository) {
			r.ProtectionRaw = classic(`{"strict":false,"contexts":[],"checks":[{"context":"ci","app_id":123}]}`)
		})
		if v := e.never(); v.State != dbstate.MergeEvaluating {
			t.Fatalf("view = %+v", v)
		}
	})
	t.Run("ruleset check bound to another app holds", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Update("o", "n", func(r *githubtest.Repository) {
			r.RulesRaw = `[{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":false,"required_status_checks":[{"context":"ci","integration_id":123}]}}]`
		})
		if v := e.never(); v.State != dbstate.MergeEvaluating {
			t.Fatalf("view = %+v", v)
		}
	})
	t.Run("a check bound to the app that ran it merges", func(t *testing.T) {
		e := newEnv(t)
		e.run()
		e.fake.Update("o", "n", func(r *githubtest.Repository) {
			r.ProtectionRaw = classic(`{"strict":false,"contexts":[],"checks":[{"context":"ci","app_id":9}]}`)
		})
		v, err := e.svc.Merge(context.Background(), e.input("r1"))
		if err != nil || v.State != dbstate.MergeMerged || e.puts() != 1 {
			t.Fatalf("view = %+v, %v, puts %d", v, err, e.puts())
		}
	})
	t.Run("classic strict", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Update("o", "n", func(r *githubtest.Repository) {
			r.ProtectionRaw = classic(`{"strict":true,"contexts":["ci"],"checks":[]}`)
		})
		e.unavailable("r1", merge.ReasonStrictChecks)
	})
	t.Run("ruleset strict", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Update("o", "n", func(r *githubtest.Repository) {
			r.RulesRaw = `[{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":true,"required_status_checks":[{"context":"ci"}]}}]`
		})
		e.unavailable("r1", merge.ReasonStrictChecks)
	})
	t.Run("classic required review", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Update("o", "n", func(r *githubtest.Repository) {
			r.ProtectionRaw = `{"required_status_checks":null,"required_pull_request_reviews":{"required_approving_review_count":1}}`
		})
		e.unavailable("r1", merge.ReasonProtection)
	})
	t.Run("required workflow rule", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Update("o", "n", func(r *githubtest.Repository) {
			r.RulesRaw = `[{"type":"workflows","parameters":{"workflows":[{"path":".github/workflows/x.yml","repository_id":7}]}}]`
		})
		e.unavailable("r1", merge.ReasonRuleUnsupported+":workflows")
	})
	t.Run("unknown rule type", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Update("o", "n", func(r *githubtest.Repository) { r.RulesRaw = `[{"type":"a_future_rule"}]` })
		e.unavailable("r1", merge.ReasonUnknownRule)
	})
	t.Run("required reviews in a ruleset", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Update("o", "n", func(r *githubtest.Repository) {
			r.RulesRaw = `[{"type":"pull_request","parameters":{"required_approving_review_count":1,"dismiss_stale_reviews_on_push":false,"require_code_owner_review":false,"require_last_push_approval":false,"required_review_thread_resolution":false,"allowed_merge_methods":["squash"]}}]`
		})
		v := e.prepared("r1")
		if v.Route != dbstate.RouteApproval || len(v.Summary.UnavailableReasons) == 0 || e.puts() != 0 {
			t.Fatalf("view = %+v", v)
		}
	})
	t.Run("a ruleset that bars squash", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Update("o", "n", func(r *githubtest.Repository) {
			r.RulesRaw = `[{"type":"pull_request","parameters":{"required_approving_review_count":0,"dismiss_stale_reviews_on_push":false,"require_code_owner_review":false,"require_last_push_approval":false,"required_review_thread_resolution":false,"allowed_merge_methods":["merge"]}}]`
		})
		v := e.prepared("r1")
		if v.Route != dbstate.RouteApproval || len(v.Summary.UnavailableReasons) == 0 {
			t.Fatalf("view = %+v", v)
		}
	})
	t.Run("an unreadable protection document", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Update("o", "n", func(r *githubtest.Repository) { r.ProtectionRaw = `{"required_status_checks":"yes"}` })
		e.unavailable("r1", merge.ReasonProtectionFields)
	})
	t.Run("an unknown protection field", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Update("o", "n", func(r *githubtest.Repository) {
			r.ProtectionRaw = `{"required_status_checks":null,"a_new_setting":{"enabled":true}}`
		})
		e.unavailable("r1", merge.ReasonProtectionFields)
	})
}

func TestReviewFailedSuiteCannotDisappearBehindPassingRun(t *testing.T) {
	// Ported from test_merge.py:1509: a newer passing run does not hide an unsuccessful suite.
	e := newEnv(t)
	e.run()
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		r.Suites[tip] = append(r.Suites[tip], githubtest.CheckSuite{ID: 2, AppID: 9, Conclusion: "failure"})
	})
	v, err := e.svc.Merge(context.Background(), e.input("r1"))
	if err != nil || v.State != dbstate.MergeBlocked || e.puts() != 0 {
		t.Fatalf("view = %+v, %v, puts %d", v, err, e.puts())
	}
}

func TestReviewNonSuccessChecksMustNotCountAsGreen(t *testing.T) {
	for _, conclusion := range []string{"neutral", "skipped"} {
		t.Run(conclusion, func(t *testing.T) {
			e := newEnv(t)
			e.run()
			e.fake.Update("o", "n", func(r *githubtest.Repository) {
				r.Runs[tip] = []githubtest.CheckRun{{ID: 1, AppID: 9, Name: "ci", Conclusion: conclusion}}
			})
			v, err := e.svc.Merge(context.Background(), e.input("r1"))
			if err != nil || v.State != dbstate.MergeBlocked || e.puts() != 0 {
				t.Fatalf("view = %+v, %v, puts %d", v, err, e.puts())
			}
		})
	}
}

func TestReviewWrongHeadCIIsNotEvidence(t *testing.T) {
	t.Run("run", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Update("o", "n", func(r *githubtest.Repository) {
			r.Runs[tip] = []githubtest.CheckRun{{ID: 1, AppID: 9, Name: "ci", Conclusion: "success", HeadSHA: moved}}
		})
		e.refused("r1", "github_unavailable")
	})
	t.Run("suite", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Update("o", "n", func(r *githubtest.Repository) {
			r.Suites[tip] = []githubtest.CheckSuite{{ID: 1, AppID: 9, Conclusion: "success", HeadSHA: moved}}
		})
		e.refused("r1", "github_unavailable")
	})
	t.Run("a run of a suite that is not listed", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Update("o", "n", func(r *githubtest.Repository) {
			r.Runs[tip] = []githubtest.CheckRun{{ID: 1, AppID: 9, SuiteID: 5, Name: "ci", Conclusion: "success"}}
		})
		e.refused("r1", "github_unavailable")
	})
}

func TestAbandonedSuiteIsIgnoredAfterTheGrace(t *testing.T) {
	// Ported from the abandoned-suite cases: a queued suite with no runs, older than the grace, is
	// not a pending check; a fresh one still holds the merge.
	for name, c := range map[string]struct {
		age  time.Duration
		want dbstate.MergeState
	}{
		"fresh": {time.Minute, dbstate.MergeEvaluating},
		"old":   {time.Hour, dbstate.MergeMerged},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.run()
			e.svc.Settle = 200 * time.Millisecond
			zero := 0
			e.fake.Update("o", "n", func(r *githubtest.Repository) {
				r.Suites[tip] = append(r.Suites[tip], githubtest.CheckSuite{
					ID: 2, AppID: 11, Status: "queued", RunsCount: &zero,
					CreatedAt: time.Now().Add(-c.age).UTC().Format(time.RFC3339),
				})
			})
			v, err := e.svc.Merge(context.Background(), e.input("r1"))
			if err != nil || v.State != c.want {
				t.Fatalf("view = %+v, %v", v, err)
			}
		})
	}
}

func TestReviewRulesPermission403MustNotEraseRequirements(t *testing.T) {
	// A 403 that is not the plan's own refusal fails closed, however the ruleset read goes.
	for name, mut := range map[string]func(*githubtest.Repository){
		"rules":      func(r *githubtest.Repository) { r.RulesStatus = 403 },
		"protection": func(r *githubtest.Repository) { r.ProtectionStatus = 403 },
		"rules, secondary-looking message": func(r *githubtest.Repository) {
			r.RulesStatus, r.RulesMessage = 403, "Resource not accessible by integration"
		},
		"rules, plan text with a trailing note": func(r *githubtest.Repository) {
			r.RulesStatus, r.RulesMessage = 403, planMessage+" See docs."
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.fake.Update("o", "n", mut)
			e.refused("r1", "github_unavailable")
		})
	}
}

func TestPlanRefusalOfBothReadsStillUsesTheChecks(t *testing.T) {
	e := newEnv(t)
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		r.ProtectionStatus, r.RulesStatus = 403, 403
		r.ProtectionMessage, r.RulesMessage = planMessage, planMessage
	})
	v := e.prepared("r1")
	if v.Route != dbstate.RouteAuto || len(v.Summary.UnavailableReasons) != 0 {
		t.Fatalf("view = %+v", v)
	}
}

func TestReviewMissingSquashSettingCannotBeAssumed(t *testing.T) {
	e := newEnv(t)
	e.fake.Update("o", "n", func(r *githubtest.Repository) { r.OmitSquash = true })
	e.unavailable("r1", merge.ReasonSquash)
	if v := e.never(); v.State == dbstate.MergeMerged {
		t.Fatalf("view = %+v", v)
	}
}

func TestSquashDisabledIsRefused(t *testing.T) {
	e := newEnv(t)
	e.fake.Update("o", "n", func(r *githubtest.Repository) { r.NoSquash = true })
	e.unavailable("r1", merge.ReasonSquash)
}

func TestSummaryNamesMergeResultFilesOnlyWhenInventoried(t *testing.T) {
	e := newEnv(t)
	v := e.prepared("r1")
	if v.Summary.MergeFileCount == nil || *v.Summary.MergeFileCount != 1 || v.Summary.MergeOnlyFileCount == nil || *v.Summary.MergeOnlyFileCount != 0 ||
		v.Summary.MergePathsDigest == nil || strings.TrimSpace(*v.Summary.MergePathsDigest) == "" {
		t.Fatalf("summary = %+v", v.Summary)
	}
}
