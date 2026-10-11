package db_test

import (
	"context"
	"regexp"
	"slices"
	"testing"

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
	}
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
