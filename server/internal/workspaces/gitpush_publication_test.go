package workspaces

import (
	"testing"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/pushgate"
)

// TestPublicationModeFollowsStoredPolicyAndGrant checks that the lifecycle's publication_mode and
// publication_reason reflect the same stored policy the push gate itself would check, not a
// hardcoded read-only stub.
func TestPublicationModeFollowsStoredPolicyAndGrant(t *testing.T) {
	h := newHarness(t, nil)
	w := h.running()
	row := h.row(w.WorkspaceId)

	publication := func() (api.WorkspaceLifecyclePublicationMode, *api.WorkspaceLifecyclePublicationReason) {
		got, err := h.svc.Get(bg, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.PublicationMode, got.PublicationReason
	}

	if mode, reason := publication(); mode != api.ReadOnly || reason == nil || *reason != api.Disabled {
		t.Fatalf("no push gate configured: mode %s, reason %v", mode, reason)
	}

	h.svc.GitPush = &GitPush{Gate: pushgate.NewService(h.svc.Pool, h.svc.Kagent, &localPushUpstream{}, nil), Origin: h.cfg.Git.PushOrigin}
	if mode, reason := publication(); mode != api.ReadOnly || reason == nil || *reason != api.MissingMetadata {
		t.Fatalf("no stored policy yet: mode %s, reason %v", mode, reason)
	}

	h.exec(`INSERT INTO git_branch_policies (project_id, policy) VALUES ($1, $2)`, row.ProjectID,
		`{"default_branch":"`+row.Branch+`"}`)
	if mode, reason := publication(); mode != api.ReadOnly || reason == nil || *reason != api.DefaultBranch {
		t.Fatalf("branch is the policy default: mode %s, reason %v", mode, reason)
	}

	h.exec(`UPDATE git_branch_policies SET policy = $2 WHERE project_id = $1`, row.ProjectID,
		`{"default_branch":"main","protected_patterns":["`+row.Branch+`"]}`)
	if mode, reason := publication(); mode != api.ReadOnly || reason == nil || *reason != api.ProtectedBranch {
		t.Fatalf("branch is protected: mode %s, reason %v", mode, reason)
	}

	h.exec(`UPDATE git_branch_policies SET policy = $2 WHERE project_id = $1`, row.ProjectID, `{"default_branch":"main"}`)
	if mode, reason := publication(); mode != api.ReadOnly || reason == nil || *reason != api.NoGrant {
		t.Fatalf("policy allows the branch but nothing has enrolled it: mode %s, reason %v", mode, reason)
	}

	h.exec(`INSERT INTO git_enrollments (workspace_id, repository, state, credential_id) VALUES ($1, 'oldsj/mainloop', 'published', 'cred')`, row.ID)
	h.exec(`INSERT INTO git_push_grants (workspace_id, branch, policy_version, credential_id, state, owner_id, writer_generation, native_session_id)
	        VALUES ($1, $2, 1, 'cred', 'published', $3, 1, $4)`, row.ID, row.Branch, owner, row.ID)
	if mode, reason := publication(); mode != api.Branch || reason != nil {
		t.Fatalf("a published grant backs the branch: mode %s, reason %v", mode, reason)
	}
}
