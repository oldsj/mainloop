package agentauth_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

func TestTokenIsDeterministicPrefixedAndKeyed(t *testing.T) {
	a := agentauth.Token([]byte("k1"), "cred-1")
	if a != agentauth.Token([]byte("k1"), "cred-1") {
		t.Error("token is not deterministic")
	}
	if !strings.HasPrefix(a, "ml_") || len(a) != 3+64 {
		t.Errorf("token shape = %q", a)
	}
	if a == agentauth.Token([]byte("k2"), "cred-1") || a == agentauth.Token([]byte("k1"), "cred-2") {
		t.Error("token ignores its key or id")
	}
	if h := agentauth.HashToken(a); len(h) != 64 || h == a {
		t.Errorf("hash = %q", h)
	}
}

func TestBearerToken(t *testing.T) {
	for header, want := range map[string]string{
		"Bearer ml_abc": "ml_abc", "bearer ml_abc": "ml_abc", "BEARER   ml_abc ": "ml_abc",
	} {
		if got, err := agentauth.BearerToken(header); err != nil || got != want {
			t.Errorf("BearerToken(%q) = %q, %v", header, got, err)
		}
	}
	for _, header := range []string{"", "Bearer", "Bearer ", "Basic ml_abc", "ml_abc", "Bearer ml_", "Bearer sk-foreign"} {
		if _, err := agentauth.BearerToken(header); !errors.Is(err, agentauth.ErrNoBearer) {
			t.Errorf("BearerToken(%q) err = %v, want ErrNoBearer", header, err)
		}
	}
}

func TestValidPair(t *testing.T) {
	type pair struct {
		r dbstate.BindingRole
		g dbstate.MCPGrantKind
	}
	valid := map[pair]bool{
		{dbstate.RoleMain, dbstate.GrantCoordination}:       true,
		{dbstate.RoleSupervisor, dbstate.GrantCoordination}: true,
		{dbstate.RoleSupervisor, dbstate.GrantWorkspace}:    true,
		{dbstate.RoleChild, dbstate.GrantCoordination}:      true,
		{dbstate.RoleChild, dbstate.GrantWorkspace}:         true,
		{dbstate.RoleAgent, dbstate.GrantWorkspace}:         true,
	}
	for _, r := range []dbstate.BindingRole{dbstate.RoleMain, dbstate.RoleSupervisor, dbstate.RoleChild, dbstate.RoleAgent} {
		for _, g := range []dbstate.MCPGrantKind{dbstate.GrantNone, dbstate.GrantCoordination, dbstate.GrantWorkspace} {
			if got := agentauth.ValidPair(r, g); got != valid[pair{r, g}] {
				t.Errorf("ValidPair(%s, %s) = %v", r, g, got)
			}
		}
	}
}

func TestAuthenticate(t *testing.T) {
	d := testdb.New(t)
	env := authtest.NewEnv(d.Pool)
	auth := &agentauth.Authenticator{Q: dbq.New(d.Pool)}
	ctx := context.Background()

	main := env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	got, err := auth.Authenticate(ctx, main.Token)
	if err != nil {
		t.Fatal(err)
	}
	if got.BindingID != main.SessionID || got.UserID != "owner" || got.Role != dbstate.RoleMain ||
		got.Grant != dbstate.GrantCoordination || got.Depth != 0 || got.AgentKind != "claude" {
		t.Errorf("actor = %+v", got)
	}

	// Task roles need a DepthFunc that checks the task principal; without one they are refused.
	taskAuth := &agentauth.Authenticator{Q: dbq.New(d.Pool), Depth: func(_ context.Context, row dbq.GetMCPActorRow) (int, bool, error) {
		if row.Role == dbstate.RoleChild {
			return 2, false, nil
		}
		return 1, false, nil
	}}
	sup := env.Seed(t, authtest.Agent{Role: dbstate.RoleSupervisor, Grant: dbstate.GrantWorkspace, Parent: main.SessionID})
	if got, err = taskAuth.Authenticate(ctx, sup.Token); err != nil || got.Depth != 1 {
		t.Errorf("supervisor = %+v, %v", got, err)
	}
	child := env.Seed(t, authtest.Agent{Role: dbstate.RoleChild, Grant: dbstate.GrantCoordination, Parent: sup.SessionID})
	if got, err = taskAuth.Authenticate(ctx, child.Token); err != nil || got.Depth != 2 {
		t.Errorf("child = %+v, %v", got, err)
	}
	t.Run("task roles fail closed without a depth hook", func(t *testing.T) {
		for _, s := range []authtest.Seeded{sup, child} {
			if _, err := auth.Authenticate(ctx, s.Token); !errors.Is(err, agentauth.ErrUnauthorized) {
				t.Errorf("err = %v", err)
			}
		}
	})

	t.Run("unknown token", func(t *testing.T) {
		for _, tok := range []string{"ml_" + strings.Repeat("0", 64), agentauth.Token([]byte("wrong key"), main.CredentialID)} {
			if _, err := auth.Authenticate(ctx, tok); !errors.Is(err, agentauth.ErrUnauthorized) {
				t.Errorf("err = %v", err)
			}
		}
	})
	t.Run("finished sessions are stale", func(t *testing.T) {
		for _, st := range []dbstate.SessionStatus{dbstate.SessionCompleted, dbstate.SessionFailed, dbstate.SessionCancelled} {
			s := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace, Status: st})
			if _, err := auth.Authenticate(ctx, s.Token); !errors.Is(err, agentauth.ErrUnauthorized) {
				t.Errorf("status %s: err = %v", st, err)
			}
		}
		for _, st := range []dbstate.SessionStatus{dbstate.SessionIdle, dbstate.SessionWaitingOnUser, dbstate.SessionSuperseded} {
			s := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace, Status: st})
			if _, err := auth.Authenticate(ctx, s.Token); err != nil {
				t.Errorf("status %s: err = %v", st, err)
			}
		}
	})
	t.Run("archived session is stale", func(t *testing.T) {
		s := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace, Archived: true})
		if _, err := auth.Authenticate(ctx, s.Token); !errors.Is(err, agentauth.ErrUnauthorized) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("grant changed to an invalid pair", func(t *testing.T) {
		if _, err := dbq.New(d.Pool).SetBindingGrant(ctx, dbq.SetBindingGrantParams{SessionID: main.SessionID, MCPGrantKind: dbstate.GrantWorkspace}); err != nil {
			t.Fatal(err)
		}
		if _, err := auth.Authenticate(ctx, main.Token); !errors.Is(err, agentauth.ErrUnauthorized) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("revoked credential", func(t *testing.T) {
		s := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
		if n, err := env.Creds.Revoke(ctx, s.SessionID); err != nil || n != 1 {
			t.Fatalf("Revoke = %d, %v", n, err)
		}
		if _, err := auth.Authenticate(ctx, s.Token); !errors.Is(err, agentauth.ErrUnauthorized) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("depth hook", func(t *testing.T) {
		s := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
		hooked := &agentauth.Authenticator{Q: dbq.New(d.Pool), Depth: func(context.Context, dbq.GetMCPActorRow) (int, bool, error) {
			return 0, true, nil
		}}
		if got, err := hooked.Authenticate(ctx, s.Token); err != nil || !got.MergeStatusOnly {
			t.Errorf("actor = %+v, %v", got, err)
		}
		failing := &agentauth.Authenticator{Q: dbq.New(d.Pool), Depth: func(context.Context, dbq.GetMCPActorRow) (int, bool, error) {
			return 0, false, errors.New("boom")
		}}
		if _, err := failing.Authenticate(ctx, s.Token); !errors.Is(err, agentauth.ErrUnauthorized) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestPolicyTable(t *testing.T) {
	want := func(names ...string) string { return strings.Join(names, ",") }
	main := agentauth.Actor{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination}
	sup := agentauth.Actor{Role: dbstate.RoleSupervisor, Grant: dbstate.GrantCoordination, Depth: 1}
	supWS := agentauth.Actor{Role: dbstate.RoleSupervisor, Grant: dbstate.GrantWorkspace, Depth: 1}
	child := agentauth.Actor{Role: dbstate.RoleChild, Grant: dbstate.GrantCoordination, Depth: 2}
	ws := agentauth.Actor{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace}
	off, on := agentauth.Policy{}, agentauth.Policy{MergeTools: true}
	join := func(p agentauth.Policy, a agentauth.Actor, s agentauth.Surface) string {
		return strings.Join(p.Tools(a, s), ",")
	}
	ord := agentauth.SurfaceOrdinary

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"main", join(off, main, ord), want("whoami", "topics", "topic_open", "note", "decide", "pending_add", "pending_done", "open_pull_request", "get_issue", "list_dependabot_alerts", "task_get", "task_list", "task_history", "delegate", "task_cancel")},
		{"supervisor coordination", join(off, sup, ord), want("whoami", "task_get", "task_list", "task_history", "delegate", "task_cancel", "report")},
		{"supervisor workspace", join(off, supWS, ord), want("whoami", "open_pull_request", "get_issue", "list_dependabot_alerts", "task_get", "task_list", "task_history", "delegate", "task_cancel", "report")},
		{"child", join(off, child, ord), want("whoami", "task_get", "task_list", "task_history", "report")},
		{"workspace agent", join(off, ws, ord), want("whoami", "open_pull_request", "get_issue", "list_dependabot_alerts")},
		{"workspace agent with merge", join(on, ws, ord), want("whoami", "open_pull_request", "get_issue", "list_dependabot_alerts", "prepare_pull_request_merge", "merge_pull_request", "get_pull_request_merge_status")},
		{"workspace agent approval", join(on, ws, agentauth.SurfaceApproval), "merge_pull_request_with_approval"},
		{"approval off", join(off, ws, agentauth.SurfaceApproval), ""},
		{"child has no merge", join(on, child, agentauth.SurfaceApproval), ""},
		{"main without grant", join(on, agentauth.Actor{Role: dbstate.RoleMain, Grant: dbstate.GrantNone}, ord), ""},
		{"main at depth 1", join(on, agentauth.Actor{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination, Depth: 1}, ord), ""},
		{"merge status only off", join(off, agentauth.Actor{MergeStatusOnly: true, Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace}, ord), ""},
		{"merge status only on", join(on, agentauth.Actor{MergeStatusOnly: true, Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace}, ord), want("whoami", "task_get", "report", "get_pull_request_merge_status")},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.name, c.got, c.want)
		}
	}
	if !on.Allowed(ws, agentauth.SurfaceApproval, agentauth.ToolMergePullRequestApproval) || off.Allowed(ws, ord, agentauth.ToolMergePullRequest) {
		t.Error("Allowed disagrees with Tools")
	}
}

func TestIssueIsIdempotentAndPublishesTheSecret(t *testing.T) {
	d := testdb.New(t)
	env := authtest.NewEnv(d.Pool)
	ctx := context.Background()
	s := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})

	again, err := env.Creds.Issue(ctx, s.SessionID, dbstate.CredentialMCP)
	if err != nil || again.CredentialID != s.CredentialID {
		t.Fatalf("second Issue = %+v, %v", again, err)
	}
	if again.SecretName != "mainloop-mcp-"+s.SessionID {
		t.Errorf("secret name = %q", again.SecretName)
	}
	if env.Cluster.UID(authtest.Namespace, again.SecretName) != again.SecretUID || again.SecretUID == "" {
		t.Error("the Secret and the recorded UID disagree")
	}
	row, _ := dbq.New(d.Pool).GetCredential(ctx, s.CredentialID)
	if row.State != dbstate.CredentialPublished || row.TokenHash != agentauth.HashToken(s.Token) {
		t.Errorf("row = %+v", row)
	}
	if strings.Contains(row.TokenHash, "ml_") {
		t.Error("the plaintext bearer is stored")
	}
}

// gatedStore holds Create after the Secret exists, so a test can act between the Secret landing
// and the credential row being marked published.
type gatedStore struct {
	agentauth.SecretStore
	created chan struct{}
	release chan struct{}
}

func (g *gatedStore) Create(ctx context.Context, name, purpose, bearer string) (string, error) {
	uid, err := g.SecretStore.Create(ctx, name, purpose, bearer)
	close(g.created)
	<-g.release
	return uid, err
}

func TestRevokeDuringPublishDoesNotLeakTheSecret(t *testing.T) {
	d := testdb.New(t)
	env := authtest.NewEnv(d.Pool)
	ctx := context.Background()
	q := dbq.New(d.Pool)
	s := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	// Start over: revoke the seeded credential, let cleanup finish, then issue a new one with a
	// gated Create.
	if _, err := env.Creds.Revoke(ctx, s.SessionID); err != nil {
		t.Fatal(err)
	}
	runCleanup(t, env, func() bool {
		got, _ := q.GetCredential(ctx, s.CredentialID)
		return got.State == dbstate.CredentialDeleted
	})
	name := "mainloop-mcp-" + s.SessionID
	gate := &gatedStore{SecretStore: env.Creds.Secrets, created: make(chan struct{}), release: make(chan struct{})}
	gated := *env.Creds
	gated.Secrets = gate
	done := make(chan error, 1)
	go func() { _, err := gated.Issue(ctx, s.SessionID, dbstate.CredentialMCP); done <- err }()
	<-gate.created
	// The Secret exists; revoke and let cleanup mark the row deleted before the publish lands.
	if _, err := env.Creds.Revoke(ctx, s.SessionID); err != nil {
		t.Fatal(err)
	}
	live, err := q.ListCredentialsByBinding(ctx, s.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	newest := live[len(live)-1]
	if _, err := d.Pool.Exec(ctx, `UPDATE credentials SET state = 'deleted' WHERE id = $1`, newest.ID); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	if err := <-done; !errors.Is(err, agentauth.ErrRevoked) {
		t.Fatalf("Issue err = %v, want ErrRevoked", err)
	}
	if uid := env.Cluster.UID(authtest.Namespace, name); uid != "" {
		t.Errorf("the Secret %s leaked (uid %s)", name, uid)
	}
	// And the binding can be issued a credential again.
	if _, err := env.Creds.Issue(ctx, s.SessionID, dbstate.CredentialMCP); err != nil {
		t.Errorf("reissue: %v", err)
	}
}

func TestRevokeInTxRollsBackWithTheTransaction(t *testing.T) {
	d := testdb.New(t)
	env := authtest.NewEnv(d.Pool)
	ctx := context.Background()
	s := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})

	rollback := errors.New("rollback")
	err := db.InTx(ctx, d.Pool, func(tx pgx.Tx) error {
		if n, err := agentauth.RevokeInTx(ctx, dbq.New(tx), s.SessionID); err != nil || n != 1 {
			t.Errorf("RevokeInTx = %d, %v", n, err)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if got, _ := dbq.New(d.Pool).GetCredential(ctx, s.CredentialID); got.State != dbstate.CredentialPublished {
		t.Fatalf("state after rollback = %s", got.State)
	}
	if err := db.InTx(ctx, d.Pool, func(tx pgx.Tx) error {
		_, err := agentauth.RevokeInTx(ctx, dbq.New(tx), s.SessionID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := dbq.New(d.Pool).GetCredential(ctx, s.CredentialID); got.State != dbstate.CredentialRevoked {
		t.Fatalf("state after commit = %s", got.State)
	}
}

func TestUnconfiguredStoreCannotIssue(t *testing.T) {
	d := testdb.New(t)
	env := authtest.NewEnv(d.Pool)
	ctx := context.Background()
	s := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	if _, err := env.Creds.Revoke(ctx, s.SessionID); err != nil {
		t.Fatal(err)
	}
	runCleanup(t, env, func() bool {
		got, _ := dbq.New(d.Pool).GetCredential(ctx, s.CredentialID)
		return got.State == dbstate.CredentialDeleted
	})
	off := *env.Creds
	off.Secrets = agentauth.Unconfigured{}
	if _, err := off.Issue(ctx, s.SessionID, dbstate.CredentialMCP); !errors.Is(err, agentauth.ErrNotConfigured) {
		t.Errorf("Issue err = %v", err)
	}
	if rows, _ := dbq.New(d.Pool).ListCredentialsByBinding(ctx, s.SessionID); len(rows) != 1 {
		t.Errorf("Issue left %d credential rows, want the 1 it started with", len(rows))
	}
}

func TestIssueRefusesAnInvalidPairAndAPendingCleanup(t *testing.T) {
	d := testdb.New(t)
	env := authtest.NewEnv(d.Pool)
	ctx := context.Background()
	bad := env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantNone})
	if _, err := env.Creds.Issue(ctx, bad.SessionID, dbstate.CredentialMCP); !errors.Is(err, agentauth.ErrInvalidGrant) {
		t.Errorf("err = %v, want ErrInvalidGrant", err)
	}
	// Git credentials do not depend on the MCP grant.
	if _, err := env.Creds.Issue(ctx, bad.SessionID, dbstate.CredentialGitRead); err != nil {
		t.Errorf("git-read Issue: %v", err)
	}

	s := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	if _, err := env.Creds.Revoke(ctx, s.SessionID); err != nil {
		t.Fatal(err)
	}
	// The old Secret is still owed a delete, so the name is not free yet.
	if _, err := env.Creds.Issue(ctx, s.SessionID, dbstate.CredentialMCP); !errors.Is(err, agentauth.ErrCleanupPending) {
		t.Errorf("err = %v, want ErrCleanupPending", err)
	}
}

// runCleanup runs the registered cleanup loop until pred holds.
func runCleanup(t *testing.T, env *authtest.Env, pred func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := reconcile.NewRunner(env.Pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	env.Creds.Register(r)
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	deadline := time.Now().Add(20 * time.Second)
	for !pred() {
		if time.Now().After(deadline) {
			t.Fatal("cleanup did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestRevokeCleansOnlyThatBinding(t *testing.T) {
	d := testdb.New(t)
	env := authtest.NewEnv(d.Pool)
	ctx := context.Background()
	q := dbq.New(d.Pool)
	a := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	b := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	if _, err := env.Creds.Issue(ctx, a.SessionID, dbstate.CredentialGitPush); err != nil {
		t.Fatal(err)
	}
	nameA, nameB := "mainloop-mcp-"+a.SessionID, "mainloop-mcp-"+b.SessionID
	uidB := env.Cluster.UID(authtest.Namespace, nameB)

	n, err := env.Creds.Revoke(ctx, a.SessionID)
	if err != nil || n != 2 {
		t.Fatalf("Revoke = %d, %v; want a's two credentials", n, err)
	}
	if got, _ := q.GetCredential(ctx, b.CredentialID); got.State != dbstate.CredentialPublished {
		t.Fatalf("b's credential is %s after a's revoke", got.State)
	}

	runCleanup(t, env, func() bool {
		rows, _ := q.ListCredentialsByBinding(ctx, a.SessionID)
		for _, r := range rows {
			if r.State != dbstate.CredentialDeleted {
				return false
			}
		}
		return len(rows) == 2
	})
	if env.Cluster.UID(authtest.Namespace, nameA) != "" {
		t.Error("a's Secret survived")
	}
	if env.Cluster.UID(authtest.Namespace, nameB) != uidB {
		t.Error("b's Secret was touched")
	}
	if got, _ := q.GetCredential(ctx, b.CredentialID); got.State != dbstate.CredentialPublished {
		t.Errorf("b's credential is %s", got.State)
	}
	for _, del := range env.Cluster.Deletes {
		if strings.HasSuffix(del, "@") {
			t.Errorf("delete without a UID precondition: %s", del)
		}
		if strings.HasPrefix(del, nameB) {
			t.Errorf("delete aimed at b's Secret: %s", del)
		}
	}
	// With the old Secret gone, the binding can be enrolled again.
	if _, err := env.Creds.Issue(ctx, a.SessionID, dbstate.CredentialMCP); err != nil {
		t.Errorf("re-Issue after cleanup: %v", err)
	}
}

func TestCleanupSparesAReplacedSecret(t *testing.T) {
	d := testdb.New(t)
	env := authtest.NewEnv(d.Pool)
	ctx := context.Background()
	q := dbq.New(d.Pool)
	s := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	name := "mainloop-mcp-" + s.SessionID
	if _, err := env.Creds.Revoke(ctx, s.SessionID); err != nil {
		t.Fatal(err)
	}
	// The Secret was replaced out of band under the same name before cleanup ran: the delete
	// carries the recorded UID, so the newcomer survives and the credential is still marked done.
	if err := env.Cluster.Replace(authtest.Namespace, name); err != nil {
		t.Fatal(err)
	}
	fresh := env.Cluster.UID(authtest.Namespace, name)
	runCleanup(t, env, func() bool {
		got, _ := q.GetCredential(ctx, s.CredentialID)
		return got.State == dbstate.CredentialDeleted
	})
	if env.Cluster.UID(authtest.Namespace, name) != fresh {
		t.Error("cleanup deleted a Secret it did not create")
	}
}

func TestCleanupFindsASecretWhosePublishNeverRecorded(t *testing.T) {
	d := testdb.New(t)
	env := authtest.NewEnv(d.Pool)
	ctx := context.Background()
	q := dbq.New(d.Pool)
	s := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	// Simulate a crash between the Secret create and the UID being recorded: planned, no UID, but
	// the Secret exists. Age it past the stray threshold so the sweep revokes it.
	if _, err := d.Pool.Exec(ctx, `UPDATE credentials SET state='planned', secret_uid=NULL, updated_at = now() - interval '1 hour' WHERE id=$1`, s.CredentialID); err != nil {
		t.Fatal(err)
	}
	if err := env.Creds.RevokeStrays(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := q.GetCredential(ctx, s.CredentialID); got.State != dbstate.CredentialRevoked {
		t.Fatalf("state = %s", got.State)
	}
	runCleanup(t, env, func() bool {
		got, _ := q.GetCredential(ctx, s.CredentialID)
		return got.State == dbstate.CredentialDeleted
	})
	if env.Cluster.UID(authtest.Namespace, "mainloop-mcp-"+s.SessionID) != "" {
		t.Error("the unrecorded Secret survived")
	}
}

func TestRevokeStraysTakesOrphansButNotFreshWork(t *testing.T) {
	d := testdb.New(t)
	env := authtest.NewEnv(d.Pool)
	ctx := context.Background()
	q := dbq.New(d.Pool)
	live := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	gone := env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	if _, err := d.Pool.Exec(ctx, `DELETE FROM sessions WHERE id=$1`, gone.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := env.Creds.RevokeStrays(ctx); err != nil { // too recent
		t.Fatal(err)
	}
	if got, _ := q.GetCredential(ctx, gone.CredentialID); got.State != dbstate.CredentialPublished {
		t.Fatalf("a fresh orphan was revoked: %s", got.State)
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE credentials SET updated_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if err := env.Creds.RevokeStrays(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := q.GetCredential(ctx, gone.CredentialID); got.State != dbstate.CredentialRevoked {
		t.Errorf("orphan is %s", got.State)
	}
	if got, _ := q.GetCredential(ctx, live.CredentialID); got.State != dbstate.CredentialPublished {
		t.Errorf("a credential with a binding is %s", got.State)
	}
}
