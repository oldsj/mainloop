package merge_test

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/merge"
)

// These tests re-create the G10 review probes for consent, authority and pinning (findings 2 to 5)
// and port the Python cases they cite. None of them may send the PUT.

// freezeAuto starts an automatic merge with the loop stopped, so its intent is committed to evaluate
// and nothing has read GitHub yet.
func (e *env) freezeAuto() string {
	e.t.Helper()
	e.svc.Settle = 0
	v, err := e.svc.Merge(context.Background(), e.input("r1"))
	if err != nil || v.State != dbstate.MergeEvaluating {
		e.t.Fatalf("freeze = %+v, %v", v, err)
	}
	return v.ProposalID
}

// freezeApproved records the owner's exact consent and starts the evaluation with the loop stopped.
func (e *env) freezeApproved() string {
	e.t.Helper()
	v := e.approvalProposal()
	e.consent(v.ProposalID, "good", nil)
	e.svc.Settle = 0
	got, err := e.approved(v.ProposalID, "good")
	if err != nil || got.State != dbstate.MergeEvaluating {
		e.t.Fatalf("freeze = %+v, %v", got, err)
	}
	return v.ProposalID
}

// stops runs the loop and requires the proposal to end in state with the detail and no PUT.
func (e *env) stops(proposal string, state dbstate.MergeState, detail string) {
	e.t.Helper()
	e.run()
	if got := e.settled(proposal); got != state || e.detail(proposal) != detail || e.puts() != 0 {
		e.t.Fatalf("state = %s (%s), puts %d; want %s (%s)", got, e.detail(proposal), e.puts(), state, detail)
	}
}

func refusalOf(err error) string {
	var r *merge.Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// bumpPolicy is a policy write: the new policy and the next version, as SetMergePolicy does.
func (e *env) bumpPolicy(policy string) {
	e.exec(`UPDATE projects SET merge_policy = $1, merge_policy_version = merge_policy_version + 1 WHERE id = 'p1'`, policy)
}

func TestReviewPolicyChangeMustInvalidateEvaluation(t *testing.T) {
	t.Parallel(
	// Ported from test_merge.py:2288 and test_merge_acceptance.py:556: a policy write after the
	// intent was frozen ends the evaluation, whichever route it took.
	)

	t.Run("automatic", func(t *testing.T) {
		e := newEnv(t)
		p := e.freezeAuto()
		e.bumpPolicy("approval")
		e.stops(p, dbstate.MergeBlocked, "policy_changed")
	})
	t.Run("approved", func(t *testing.T) {
		e := newEnv(t)
		p := e.freezeApproved()
		e.bumpPolicy("approval")
		e.stops(p, dbstate.MergeBlocked, "policy_changed")
	})
	t.Run("project gone", func(t *testing.T) {
		e := newEnv(t)
		p := e.freezeAuto()
		e.exec(`UPDATE projects SET user_id = 'someone-else' WHERE id = 'p1'`)
		e.stops(p, dbstate.MergeBlocked, "project_unavailable")
	})
}

func TestPolicyWriteInFlightOrdersWithTheFinalDecision(t *testing.T) {
	t.Parallel(
	// A policy write holds the project row until it commits. The final decision waits for it and then
	// sees the new version, so the merge can never read the old policy after the write began.
	)

	e := newEnv(t)
	p := e.freezeAuto()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM projects WHERE id = 'p1' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	e.run()
	time.Sleep(300 * time.Millisecond)
	if s := e.state(p); s != dbstate.MergeEvaluating || e.puts() != 0 {
		t.Fatalf("decided past a policy write: %s, puts %d", s, e.puts())
	}
	if _, err := tx.Exec(ctx, `UPDATE projects SET merge_policy = 'approval', merge_policy_version = merge_policy_version + 1 WHERE id = 'p1'`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.settled(p); got != dbstate.MergeBlocked || e.detail(p) != "policy_changed" || e.puts() != 0 {
		t.Fatalf("state = %s (%s), puts %d", got, e.detail(p), e.puts())
	}
}

func TestReviewReplacementRuntimeMustInvalidateConsent(t *testing.T) {
	t.Parallel(
	// Ported from test_merge_acceptance.py:571: consent belongs to the runtime it was given for.
	)

	t.Run("before the call", func(t *testing.T) {
		e := newEnv(t)
		v := e.approvalProposal()
		e.consent(v.ProposalID, "good", nil)
		e.exec(`UPDATE agent_bindings SET kagent_session_id = 'replacement' WHERE session_id = 's1'`)
		if _, err := e.approved(v.ProposalID, "good"); refusalOf(err) != "approval_required" || e.puts() != 0 {
			t.Fatalf("err = %v, puts %d", err, e.puts())
		}
	})
	t.Run("after evaluation began", func(t *testing.T) {
		e := newEnv(t)
		p := e.freezeApproved()
		e.exec(`UPDATE agent_bindings SET kagent_session_id = 'replacement' WHERE session_id = 's1'`)
		e.stops(p, dbstate.MergeBlocked, "consent_runtime_changed")
	})
}

func TestReviewArchivedSessionMustNotCompleteApprovedMerge(t *testing.T) {
	t.Parallel()
	for name, kill := range map[string]string{
		"archived":        `UPDATE sessions SET archived_at = now() WHERE id = 's1'`,
		"runtime deleted": `UPDATE agent_bindings SET kagent_deleted_at = now() WHERE session_id = 's1'`,
	} {
		t.Run(name+" before the sweep", func(t *testing.T) {
			e := newEnv(t)
			v := e.approvalProposal()
			e.consent(v.ProposalID, "good", nil)
			e.exec(kill)
			if err := e.svc.Complete(context.Background()); err != nil {
				t.Fatal(err)
			}
			e.run()
			time.Sleep(200 * time.Millisecond)
			if s := e.state(v.ProposalID); s != dbstate.MergePrepared || e.puts() != 0 {
				t.Fatalf("state = %s, puts %d", s, e.puts())
			}
		})
		t.Run(name+" after evaluation began", func(t *testing.T) {
			e := newEnv(t)
			p := e.freezeApproved()
			e.exec(kill)
			e.stops(p, dbstate.MergeBlocked, "consent_runtime_changed")
		})
	}
}

func TestReviewTemplateMappingChangeMustInvalidateConsent(t *testing.T) {
	t.Parallel()
	agent := kagent.AgentRef{Namespace: "ns", Name: "a"}
	// Ported from test_merge_authorization.py:171: an Agent with an inline template, an unknown or
	// duplicate template/provider pair, a Session that is not on the recorded Agent, or a failed read
	// gives the tool no authority.
	cases := map[string]func(e *env){
		"inline template": func(e *env) { e.rt.template(agent, "") },
		"unknown template": func(e *env) {
			e.rt.template(agent, "different-template")
		},
		"changed configuration": func(e *env) {
			c := claudeConfig
			c.TemplateName = "different-template"
			e.svc.Configs = []config.MergeConfiguration{c}
		},
		"duplicate mapping": func(e *env) {
			dup := claudeConfig
			dup.Endpoint = "http://other/mcp/merge-approval"
			e.svc.Configs = []config.MergeConfiguration{claudeConfig, dup}
		},
		"other provider": func(e *env) {
			c := claudeConfig
			c.Provider = config.NativeCodex
			e.svc.Configs = []config.MergeConfiguration{c}
		},
		"session on another agent": func(e *env) {
			e.rt.agents["k1"] = kagent.AgentRef{Namespace: "ns", Name: "other"}
		},
		"no runtime": func(e *env) { e.svc.Runtime = nil },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			v := e.approvalProposal()
			e.consent(v.ProposalID, "good", nil)
			change(e)
			if _, err := e.approved(v.ProposalID, "good"); refusalOf(err) != "approval_required" || e.puts() != 0 {
				t.Fatalf("err = %v, puts %d", err, e.puts())
			}
		})
	}
	t.Run("a failed read is not a mapping", func(t *testing.T) {
		e := newEnv(t)
		v := e.approvalProposal()
		e.consent(v.ProposalID, "good", nil)
		e.rt.err = errors.New("kagent down")
		if _, err := e.approved(v.ProposalID, "good"); err == nil || e.puts() != 0 {
			t.Fatalf("err = %v, puts %d", err, e.puts())
		}
	})
	t.Run("changed after evaluation began", func(t *testing.T) {
		e := newEnv(t)
		p := e.freezeApproved()
		e.rt.template(agent, "different-template")
		e.stops(p, dbstate.MergeBlocked, "consent_mapping_changed")
	})
	t.Run("the mapping evidence is kept with the decision", func(t *testing.T) {
		e := newEnv(t)
		p := e.freezeApproved()
		var tmpl, provider, digest, session string
		if err := e.pool.QueryRow(context.Background(),
			`SELECT consent_pin->>'template_name', consent_pin->>'provider', consent_pin->>'config_digest', consent_pin->>'kagent_session_id'
			FROM merge_candidates c JOIN merge_proposals p ON p.candidate_id = c.id WHERE p.id = $1`, p).Scan(&tmpl, &provider, &digest, &session); err != nil {
			t.Fatal(err)
		}
		if tmpl != "t" || provider != "claude" || session != "k1" || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(digest) {
			t.Fatalf("pin = %q %q %q %q", tmpl, provider, digest, session)
		}
		e.run()
		if got := e.settled(p); got != dbstate.MergeMerged || e.puts() != 1 {
			t.Fatalf("state = %s, puts %d", got, e.puts())
		}
	})
}

func TestReviewConsentWithdrawnDuringEvidenceMustPreventPUT(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.freezeApproved()
	// The last evidence read; the withdrawal lands after the pre-read check and before the decision.
	e.fake.Before("GET", "/repos/o/n/commits/"+tip+"/statuses", func() {
		e.exec(`UPDATE hitl_responses SET state = 'rejected_transport'`)
	})
	e.stops(p, dbstate.MergeBlocked, "consent_withdrawn")
}

func TestReviewDeadlinePassingDuringEvidenceMustPreventPUT(t *testing.T) {
	t.Parallel()
	for name, freeze := range map[string]func(*env) string{
		"automatic": (*env).freezeAuto,
		"approved":  (*env).freezeApproved,
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			p := freeze(e)
			e.fake.Before("GET", "/repos/o/n/commits/"+tip+"/statuses", func() { e.now.Store(11 * 60) })
			e.stops(p, dbstate.MergeExpired, "deadline_passed")
		})
	}
}

func TestReviewPinnedFactsMustNotChange(t *testing.T) {
	t.Parallel(
	// Ported from services/merge.py:813: the owner approved one repository, base and presentation.
	)

	cases := map[string]struct {
		change func(r *githubtest.Repository)
		detail string
	}{
		"repository id": {func(r *githubtest.Repository) { r.ID = 8 }, "facts_changed"},
		"base sha": {func(r *githubtest.Repository) {
			r.Branches[r.DefaultBranch] = baseTip
		}, "default_branch_moved"},
		"default branch and base": {func(r *githubtest.Repository) {
			r.Branches["trunk"] = githubtest.TipOf(r, r.DefaultBranch)
			r.DefaultBranch = "trunk"
			r.PullRequests[0].BaseRef = "trunk"
		}, "facts_changed"},
		"title": {func(r *githubtest.Repository) { r.PullRequests[0].Title = "Something else" }, "facts_changed"},
		"body":  {func(r *githubtest.Repository) { r.PullRequests[0].Body = "Something else" }, "facts_changed"},
		"file facts": {func(r *githubtest.Repository) {
			r.PullRequests[0].Files = []githubtest.File{{Filename: "server/a.go", Additions: 30, Deletions: 1}}
		}, "facts_changed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			p := e.freezeApproved()
			e.fake.Update("o", "n", tc.change)
			e.stops(p, dbstate.MergeBlocked, tc.detail)
		})
	}
}

func TestDefaultBranchMovingAfterEvidenceMustPreventPUT(t *testing.T) {
	t.Parallel(
	// The tip is read again just before the decision: it moves after the evidence read and before it.
	)

	e := newEnv(t)
	p := e.freezeApproved()
	n := 0
	e.fake.Before("GET", "/repos/o/n/commits/"+tip+"/statuses", func() {
		n++
		e.fake.Update("o", "n", func(r *githubtest.Repository) {
			r.Branches[r.DefaultBranch] = baseTip
			r.Compares = map[string]githubtest.Compare{}
		})
	})
	e.stops(p, dbstate.MergeBlocked, "default_branch_moved")
	if n != 1 {
		t.Fatalf("hook ran %d times", n)
	}
}

func TestReviewStatusAndApprovalMustBeProjectScoped(t *testing.T) {
	t.Parallel(
	// A session bound to another project, or to none, cannot read or approve a proposal of this one,
	// even with the right owner, request id and session.
	)

	e := newEnv(t)
	v := e.approvalProposal()
	e.consent(v.ProposalID, "good", nil)
	ctx := context.Background()
	for _, project := range []string{"", "p2"} {
		if _, err := e.svc.Status(ctx, owner, project, v.ProposalID, "prep"); refusalOf(err) != "forbidden" {
			t.Fatalf("status for project %q: %v", project, err)
		}
		_, err := e.svc.MergeApproved(ctx, merge.ApprovedInput{OwnerID: owner, ActorProjectID: project, SessionID: "s1", ProposalID: v.ProposalID, RequestID: "good"})
		if refusalOf(err) != "forbidden" {
			t.Fatalf("approval for project %q: %v", project, err)
		}
	}
	if e.puts() != 0 {
		t.Fatalf("puts = %d", e.puts())
	}
	if _, err := e.svc.Status(ctx, owner, "p1", v.ProposalID, "prep"); err != nil {
		t.Fatalf("status from the right project: %v", err)
	}
}
