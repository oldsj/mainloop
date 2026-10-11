package db_test

import (
	"context"
	"reflect"
	"regexp"
	"slices"
	"testing"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

// The Go constants and the CHECK constraints must name exactly the same values. TestStateChecks
// catches a constant missing from a CHECK; this also catches a CHECK value with no constant.
func TestStateConstantsMatchChecks(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	quoted := regexp.MustCompile(`'([^']*)'::text`)
	strs := func(v ...string) []string { return v }
	taskReasons := strs(
		string(dbstate.ReasonOwnerDecision), string(dbstate.ReasonAwaitingChild), string(dbstate.ReasonApproval),
		string(dbstate.ReasonCI), string(dbstate.ReasonPublication), string(dbstate.ReasonHandoff),
		string(dbstate.ReasonReconciliation), string(dbstate.ReasonProvisioningUnavailable),
		string(dbstate.ReasonHandoffUnavailable), string(dbstate.ReasonCancelUnavailable))
	taskOpStates := strs(
		string(dbstate.TaskOpRequested), string(dbstate.TaskOpDraining), string(dbstate.TaskOpCheckpointRequired),
		string(dbstate.TaskOpCheckpointVerified), string(dbstate.TaskOpSourceFencing), string(dbstate.TaskOpSourceFenced),
		string(dbstate.TaskOpTargetCreating), string(dbstate.TaskOpTargetReady), string(dbstate.TaskOpCompleted),
		string(dbstate.TaskOpBlocked), string(dbstate.TaskOpUncertain))
	cases := []struct {
		table, column string
		values        []string
	}{
		{"projects", "merge_policy", strs(string(dbstate.MergeAuto), string(dbstate.MergeApproval))},
		{"sessions", "status", strs(
			string(dbstate.SessionPending), string(dbstate.SessionActive), string(dbstate.SessionIdle),
			string(dbstate.SessionWaitingOnUser), string(dbstate.SessionSuperseded),
			string(dbstate.SessionCompleted), string(dbstate.SessionFailed), string(dbstate.SessionCancelled))},
		{"agent_bindings", "role", strs(
			string(dbstate.RoleMain), string(dbstate.RoleChild), string(dbstate.RoleAgent), string(dbstate.RoleSupervisor))},
		{"agent_bindings", "mcp_grant_kind", strs(
			string(dbstate.GrantNone), string(dbstate.GrantCoordination), string(dbstate.GrantWorkspace))},
		{"credentials", "kind", strs(
			string(dbstate.CredentialMCP), string(dbstate.CredentialGitRead), string(dbstate.CredentialGitPush))},
		{"credentials", "state", strs(
			string(dbstate.CredentialPlanned), string(dbstate.CredentialPublished),
			string(dbstate.CredentialRevoked), string(dbstate.CredentialDeleted))},
		{"topic_records", "kind", strs(
			string(dbstate.RecordNote), string(dbstate.RecordDecision), string(dbstate.RecordPending), string(dbstate.RecordReport))},
		{"topic_records", "status", strs(string(dbstate.RecordOpen), string(dbstate.RecordDone))},
		{"environment_versions", "validation_status", strs(
			string(dbstate.ValidationPendingBuild), string(dbstate.ValidationStaticValidated))},
		{"environment_versions", "provenance_kind", strs(
			string(dbstate.ProvenanceUserPushed), string(dbstate.ProvenanceMainloopBuilt), string(dbstate.ProvenanceOperator))},
		{"environment_versions", "base_provenance", strs(
			string(dbstate.BaseSupplied), string(dbstate.BaseVerified), string(dbstate.BaseUnknown))},
		{"dev_environments", "source_kind", strs(
			string(dbstate.SourcePrebuiltImage), string(dbstate.SourceDefinitionRepo), string(dbstate.SourceOperatorDefault))},
		{"environment_grants", "permission", strs(string(dbstate.PermissionUse), string(dbstate.PermissionDerive))},
		{"workspace_ops", "kind", strs(
			string(dbstate.OpCreate), string(dbstate.OpSuspend), string(dbstate.OpResume), string(dbstate.OpDelete))},
		{"workspace_ops", "state", strs(
			string(dbstate.OpPending), string(dbstate.OpRetrying), string(dbstate.OpSucceeded),
			string(dbstate.OpRejected), string(dbstate.OpGaveUp), string(dbstate.OpCancelled))},
		{"project_merge_policy_audit", "old_policy", strs(string(dbstate.MergeAuto), string(dbstate.MergeApproval))},
		{"project_merge_policy_audit", "new_policy", strs(string(dbstate.MergeAuto), string(dbstate.MergeApproval))},
		{"deliveries", "state", strs(
			string(dbstate.DeliveryRecorded), string(dbstate.DeliverySending), string(dbstate.DeliveryDelivered),
			string(dbstate.DeliveryCompleted), string(dbstate.DeliveryFailed), string(dbstate.DeliveryCancelled),
			string(dbstate.DeliveryUncertain))},
		{"tasks", "mode", strs(string(dbstate.TaskModeCode), string(dbstate.TaskModeCoordination))},
		{"tasks", "selection_source", strs(
			string(dbstate.SelectionExplicit), string(dbstate.SelectionProjectDefault),
			string(dbstate.SelectionInstallationDefault), string(dbstate.SelectionInherited))},
		{"tasks", "status", strs(
			string(dbstate.TaskQueued), string(dbstate.TaskRunning), string(dbstate.TaskWaiting),
			string(dbstate.TaskBlocked), string(dbstate.TaskCompleted), string(dbstate.TaskFailed),
			string(dbstate.TaskCancelled))},
		{"tasks", "reason", taskReasons},
		{"task_attempts", "native_provider", strs(string(dbstate.ProviderClaude), string(dbstate.ProviderCodex))},
		{"task_attempts", "role", strs(string(dbstate.AttemptSupervisor), string(dbstate.AttemptChild))},
		{"task_attempts", "state", strs(
			string(dbstate.AttemptCreating), string(dbstate.AttemptActive), string(dbstate.AttemptDraining),
			string(dbstate.AttemptFenced), string(dbstate.AttemptSuperseded), string(dbstate.AttemptFailed),
			string(dbstate.AttemptCancelled), string(dbstate.AttemptCompleted))},
		{"task_operations", "kind", strs(
			string(dbstate.TaskOpCreate), string(dbstate.TaskOpRetry), string(dbstate.TaskOpReassign),
			string(dbstate.TaskOpCancel))},
		{"task_operations", "state", taskOpStates},
		{"task_operations", "last_confirmed_step", taskOpStates},
		{"task_operations", "reason", taskReasons},
		{"task_artifacts", "kind", strs(
			string(dbstate.ArtifactCheckpoint), string(dbstate.ArtifactHandoffManifest),
			string(dbstate.ArtifactProviderSummary), string(dbstate.ArtifactRetentionReceipt))},
		{"task_reports", "outcome", strs(
			string(dbstate.OutcomeProgress), string(dbstate.OutcomeCompleted), string(dbstate.OutcomeFailed),
			string(dbstate.OutcomeBlocked))},
		{"task_event_deliveries", "state", strs(
			string(dbstate.TaskDeliveryPending), string(dbstate.TaskDeliveryQueued), string(dbstate.TaskDeliveryDelivered),
			string(dbstate.TaskDeliveryUncertain), string(dbstate.TaskDeliveryCancelled))},
		{"hitl_requests", "kind", strs(string(dbstate.HITLToolApproval), string(dbstate.HITLAskUser))},
		{"hitl_requests", "availability", strs(
			string(dbstate.HITLPending), string(dbstate.HITLStale), string(dbstate.HITLUnavailable),
			string(dbstate.HITLAnswered))},
		{"hitl_responses", "state", strs(
			string(dbstate.HITLRecorded), string(dbstate.HITLSending), string(dbstate.HITLAccepted),
			string(dbstate.HITLUncertain), string(dbstate.HITLRejectedTransport))},
		{"attention_items", "kind", strs(
			string(dbstate.AttentionApproval), string(dbstate.AttentionQuestion), string(dbstate.AttentionOwnerDecision))},
		{"attention_items", "state", strs(
			string(dbstate.AttentionOpen), string(dbstate.AttentionResolved), string(dbstate.AttentionDismissed))},
		{"git_enrollments", "state", strs(
			string(dbstate.GitEnrollmentPlanned), string(dbstate.GitEnrollmentPublished), string(dbstate.GitEnrollmentRevoked))},
		{"git_push_grants", "state", strs(
			string(dbstate.GitGrantPlanned), string(dbstate.GitGrantPublished), string(dbstate.GitGrantRevoked))},
		{"health_collection", "state", strs(
			string(dbstate.HealthPending), string(dbstate.HealthCollected), string(dbstate.HealthUnavailable))},
		{"git_publications", "state", strs(
			string(dbstate.PublicationPending), string(dbstate.PublicationDispatching), string(dbstate.PublicationConfirmed),
			string(dbstate.PublicationRejected), string(dbstate.PublicationUnknown))},
		// No state machine, so no dbstate type: the contract enum is the constant the CHECK must match.
		{"environment_versions", "architecture", strs(
			string(api.EnvironmentVersionArchitectureAmd64), string(api.EnvironmentVersionArchitectureArm64))},
		{"session_messages", "role", strs(string(api.MessageRoleUser), string(api.MessageRoleAssistant))},
	}
	// Every enumerated CHECK in the schema needs a case above, so a new state column cannot be
	// added without constants that are held to it.
	t.Run("all enumerated checks covered", func(t *testing.T) {
		covered := map[string]bool{}
		for _, c := range cases {
			covered[c.table+"_"+c.column+"_check"] = true
		}
		rows, err := d.Pool.Query(ctx,
			`SELECT c.conrelid::regclass::text, c.conname FROM pg_constraint c
			 JOIN pg_namespace n ON n.oid = c.connamespace
			 WHERE n.nspname = 'mainloop' AND c.contype = 'c'
			   AND pg_get_constraintdef(c.oid) LIKE '%= ANY (ARRAY[%'
			 ORDER BY 1, 2`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var table, name string
			if err := rows.Scan(&table, &name); err != nil {
				t.Fatal(err)
			}
			if !covered[name] {
				t.Errorf("CHECK %s on %s has no case in TestStateConstantsMatchChecks", name, table)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	})
	for _, c := range cases {
		t.Run(c.table+"."+c.column, func(t *testing.T) {
			var def string
			err := d.Pool.QueryRow(ctx,
				`SELECT pg_get_constraintdef(oid) FROM pg_constraint
				 WHERE conrelid = ('mainloop.' || $1::text)::regclass AND contype = 'c' AND conname = $2`,
				c.table, c.table+"_"+c.column+"_check").Scan(&def)
			if err != nil {
				t.Fatalf("constraint %s_%s_check: %v", c.table, c.column, err)
			}
			var got []string
			for _, m := range quoted.FindAllStringSubmatch(def, -1) {
				got = append(got, m[1])
			}
			want := slices.Clone(c.values)
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("CHECK %q has %v, constants have %v", def, got, want)
			}
		})
	}
}

// watched_tag is interpolated into a registry URL path, so the database enforces the tag grammar
// for every writer.
func TestWatchedTagCheck(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	for tag, ok := range map[string]bool{"stable": true, "v1.2-rc_3": true, "_x": true, "a/b": false, "a?x": false, "..": false, "-x": false, "a\n": false, "": false} {
		_, err := d.Pool.Exec(ctx,
			`INSERT INTO mainloop.dev_environments (id, owner_id, name, source_kind, watched_tag) VALUES ($1, 'o', 'n', 'prebuilt_image', $2)`,
			"env-"+tag, tag)
		if (err == nil) != ok {
			t.Errorf("watched_tag %q: err = %v, want accepted = %v", tag, err, ok)
		}
	}
}

// Op kind and state columns come out of sqlc as the dbstate types, so the compiler checks every
// comparison and assignment against the constants that the CHECK test holds to the schema.
func TestWorkspaceOpColumnsAreTyped(t *testing.T) {
	typ := reflect.TypeFor[dbq.WorkspaceOp]()
	for field, want := range map[string]reflect.Type{
		"Kind":  reflect.TypeFor[dbstate.WorkspaceOpKind](),
		"State": reflect.TypeFor[dbstate.WorkspaceOpState](),
	} {
		f, ok := typ.FieldByName(field)
		if !ok || f.Type != want {
			t.Errorf("dbq.WorkspaceOp.%s is %v, want %v", field, f.Type, want)
		}
	}
}
