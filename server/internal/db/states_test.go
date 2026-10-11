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
