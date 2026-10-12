package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func ptr[T any](v T) *T { return &v }

const (
	pgCheckViolation  = "23514"
	pgUniqueViolation = "23505"
)

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func newProject(t *testing.T, q *dbq.Queries, id, fullName string) dbq.Project {
	t.Helper()
	owner, name, _ := strings.Cut(fullName, "/")
	p, err := q.CreateProject(context.Background(), dbq.CreateProjectParams{
		ID: id, UserID: "owner", Owner: owner, Name: name, FullName: fullName,
		DefaultBranch: "main", HTMLURL: "https://github.com/" + fullName,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newSession(t *testing.T, q *dbq.Queries, id string, project *string) dbq.Session {
	t.Helper()
	s, err := q.CreateSession(context.Background(), dbq.CreateSessionParams{
		ID: id, UserID: "owner", ProjectID: project, Title: "title", Description: "desc",
		Prompt: "prompt", BaseBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestProjectsRoundTrip(t *testing.T) {
	ctx := context.Background()
	q := dbq.New(testdb.New(t).Pool)

	p := newProject(t, q, "p1", "Acme/Widget")
	if p.MergePolicy != dbstate.MergeAuto || p.MergePolicyVersion != 1 || p.DefaultBranch != "main" {
		t.Errorf("defaults = %+v", p)
	}
	got, err := q.GetProjectByFullName(ctx, dbq.GetProjectByFullNameParams{UserID: "owner", Lower: "acme/WIDGET"})
	if err != nil || got.ID != "p1" {
		t.Fatalf("lookup by full name = %+v, %v", got, err)
	}
	// GitHub names are case-insensitive.
	_, err = q.CreateProject(ctx, dbq.CreateProjectParams{
		ID: "p2", UserID: "owner", Owner: "acme", Name: "widget", FullName: "acme/widget",
		DefaultBranch: "main", HTMLURL: "https://github.com/acme/widget",
	})
	if pgCode(err) != pgUniqueViolation {
		t.Errorf("duplicate by case: err = %v", err)
	}

	updated, err := q.UpdateProjectMetadata(ctx, dbq.UpdateProjectMetadataParams{
		ID: "p1", Description: ptr("a widget"), DefaultBranch: "trunk", OpenPRCount: 3, OpenIssueCount: 4,
	})
	if err != nil || updated.DefaultBranch != "trunk" || updated.OpenPRCount != 3 || updated.MetadataUpdatedAt == nil {
		t.Fatalf("metadata = %+v, %v", updated, err)
	}
	pol, err := q.SetProjectMergePolicy(ctx, dbq.SetProjectMergePolicyParams{ID: "p1", MergePolicy: dbstate.MergeApproval})
	if err != nil || pol.MergePolicy != dbstate.MergeApproval || pol.MergePolicyVersion != 2 {
		t.Fatalf("policy = %+v, %v", pol, err)
	}
	if err := q.TouchProject(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	list, err := q.ListProjects(ctx, "owner")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %d, %v", len(list), err)
	}
	if n, err := q.DeleteProject(ctx, "p1"); err != nil || n != 1 {
		t.Fatalf("delete = %d, %v", n, err)
	}
	if _, err := q.GetProject(ctx, "p1"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("get after delete: %v", err)
	}
}

func TestSessionsRoundTrip(t *testing.T) {
	ctx := context.Background()
	q := dbq.New(testdb.New(t).Pool)
	p := newProject(t, q, "p1", "acme/widget")

	s := newSession(t, q, "s1", &p.ID)
	if s.Status != dbstate.SessionPending || s.StartedAt != nil || s.ArchivedAt != nil || s.Result != nil {
		t.Errorf("defaults = %+v", s)
	}
	active, err := q.SetSessionStatus(ctx, dbq.SetSessionStatusParams{ID: "s1", Status: dbstate.SessionActive})
	if err != nil || active.StartedAt == nil || active.CompletedAt != nil {
		t.Fatalf("active = %+v, %v", active, err)
	}
	done, err := q.SetSessionStatus(ctx, dbq.SetSessionStatusParams{ID: "s1", Status: dbstate.SessionCompleted})
	if err != nil || done.CompletedAt == nil || done.StartedAt == nil {
		t.Fatalf("completed = %+v, %v", done, err)
	}
	out, err := q.SetSessionOutcome(ctx, dbq.SetSessionOutcomeParams{
		ID: "s1", Summary: ptr("shipped"), CommitSha: ptr("abc123"), Result: json.RawMessage(`{"files":2}`),
	})
	if err != nil || out.Summary == nil || *out.Summary != "shipped" {
		t.Fatalf("outcome = %+v, %v", out, err)
	}
	var result map[string]int
	if err := json.Unmarshal(out.Result, &result); err != nil || result["files"] != 2 {
		t.Errorf("result = %s, %v", out.Result, err)
	}

	list, err := q.ListSessions(ctx, dbq.ListSessionsParams{UserID: "owner"})
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %d, %v", len(list), err)
	}
	if n, err := q.ArchiveSession(ctx, "s1"); err != nil || n != 1 {
		t.Fatalf("archive = %d, %v", n, err)
	}
	list, _ = q.ListSessions(ctx, dbq.ListSessionsParams{UserID: "owner"})
	if len(list) != 0 {
		t.Errorf("archived session listed")
	}
	list, _ = q.ListSessions(ctx, dbq.ListSessionsParams{UserID: "owner", IncludeArchived: true})
	if len(list) != 1 {
		t.Errorf("archived session missing with include_archived")
	}
	if n, err := q.DeleteSession(ctx, "s1"); err != nil || n != 1 {
		t.Fatalf("delete = %d, %v", n, err)
	}
}

func TestAgentBindingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	q := dbq.New(testdb.New(t).Pool)
	newSession(t, q, "parent", nil)
	newSession(t, q, "child", nil)
	topic, err := q.CreateTopic(ctx, dbq.CreateTopicParams{ID: "tp", UserID: "owner", Name: "topic"})
	if err != nil {
		t.Fatal(err)
	}

	b, err := q.CreateAgentBinding(ctx, dbq.CreateAgentBindingParams{
		SessionID: "child", Kind: "claude", Role: dbstate.RoleChild, MCPGrantKind: dbstate.GrantCoordination,
		ParentSessionID: ptr("parent"), TopicID: &topic.ID, Model: ptr("sonnet"),
	})
	if err != nil || b.KagentSessionID != nil || b.QueueHeld || b.Turns != 0 {
		t.Fatalf("binding = %+v, %v", b, err)
	}
	b, err = q.SetBindingKagentSession(ctx, dbq.SetBindingKagentSessionParams{SessionID: "child", KagentSessionID: ptr("ks-1")})
	if err != nil || b.KagentSessionID == nil || *b.KagentSessionID != "ks-1" {
		t.Fatalf("set kagent session = %+v, %v", b, err)
	}
	byKagent, err := q.GetAgentBindingByKagentSession(ctx, ptr("ks-1"))
	if err != nil || byKagent.SessionID != "child" {
		t.Fatalf("by kagent session = %+v, %v", byKagent, err)
	}
	// A kagent Session belongs to one binding.
	if _, err := q.CreateAgentBinding(ctx, dbq.CreateAgentBindingParams{
		SessionID: "parent", Kind: "claude", Role: dbstate.RoleMain, MCPGrantKind: dbstate.GrantNone,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = q.SetBindingKagentSession(ctx, dbq.SetBindingKagentSessionParams{SessionID: "parent", KagentSessionID: ptr("ks-1")})
	if pgCode(err) != pgUniqueViolation {
		t.Errorf("shared kagent session: err = %v", err)
	}
	if err := q.SetBindingQueueHeld(ctx, dbq.SetBindingQueueHeldParams{SessionID: "child", QueueHeld: true}); err != nil {
		t.Fatal(err)
	}
	if err := q.MarkBindingKagentDeleted(ctx, "child"); err != nil {
		t.Fatal(err)
	}
	b, _ = q.GetAgentBinding(ctx, "child")
	if !b.QueueHeld || b.KagentDeletedAt == nil {
		t.Errorf("after updates = %+v", b)
	}
	children, err := q.ListAgentBindingsByParent(ctx, ptr("parent"))
	if err != nil || len(children) != 1 {
		t.Fatalf("children = %d, %v", len(children), err)
	}
	// Deleting a session removes its binding.
	if _, err := q.DeleteSession(ctx, "child"); err != nil {
		t.Fatal(err)
	}
	if _, err := q.GetAgentBinding(ctx, "child"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("binding survived its session: %v", err)
	}
}

func TestCredentialsLifecycle(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	q := dbq.New(d.Pool)
	hash := strings.Repeat("ab", 32)

	c, err := q.CreateCredential(ctx, dbq.CreateCredentialParams{
		ID: "c1", Kind: dbstate.CredentialMCP, BindingID: "s1", TokenHash: hash, SecretName: "mainloop-mcp-s1",
	})
	if err != nil || c.State != dbstate.CredentialPlanned || c.SecretUID != nil {
		t.Fatalf("create = %+v, %v", c, err)
	}
	// One live credential per binding and kind.
	_, err = q.CreateCredential(ctx, dbq.CreateCredentialParams{
		ID: "c2", Kind: dbstate.CredentialMCP, BindingID: "s1", TokenHash: strings.Repeat("cd", 32), SecretName: "other",
	})
	if pgCode(err) != pgUniqueViolation {
		t.Errorf("second live credential: err = %v", err)
	}

	c, err = q.MarkCredentialPublished(ctx, dbq.MarkCredentialPublishedParams{ID: "c1", SecretUID: ptr("uid-1")})
	if err != nil || c.State != dbstate.CredentialPublished || *c.SecretUID != "uid-1" {
		t.Fatalf("published = %+v, %v", c, err)
	}
	if _, err := q.MarkCredentialPublished(ctx, dbq.MarkCredentialPublishedParams{ID: "c1", SecretUID: ptr("uid-2")}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("publish twice: %v", err)
	}

	// Revoking keeps the UID recorded at publish; the delete precondition needs that one.
	c, err = q.MarkCredentialRevoked(ctx, dbq.MarkCredentialRevokedParams{ID: "c1", SecretUID: ptr("other-uid")})
	if err != nil || c.State != dbstate.CredentialRevoked || c.RevokedAt == nil || *c.SecretUID != "uid-1" {
		t.Fatalf("revoked = %+v, %v", c, err)
	}
	// While the Secret delete is owed, its name stays taken.
	_, err = q.CreateCredential(ctx, dbq.CreateCredentialParams{
		ID: "c3", Kind: dbstate.CredentialGitRead, BindingID: "s1", TokenHash: strings.Repeat("ef", 32), SecretName: "mainloop-mcp-s1",
	})
	if pgCode(err) != pgUniqueViolation {
		t.Errorf("reused Secret name before delete: err = %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE credentials SET state = 'deleted' WHERE id = 'c1'`); err != nil {
		t.Fatal(err)
	}
	// A new credential can now take the name and the binding slot.
	if _, err := q.CreateCredential(ctx, dbq.CreateCredentialParams{
		ID: "c4", Kind: dbstate.CredentialMCP, BindingID: "s1", TokenHash: strings.Repeat("12", 32), SecretName: "mainloop-mcp-s1",
	}); err != nil {
		t.Errorf("reissue after delete: %v", err)
	}
	all, err := q.ListCredentialsByBinding(ctx, "s1")
	if err != nil || len(all) != 2 {
		t.Errorf("by binding = %d, %v", len(all), err)
	}
}

func TestTopicsRoundTrip(t *testing.T) {
	ctx := context.Background()
	q := dbq.New(testdb.New(t).Pool)
	newSession(t, q, "s1", nil)

	tp, err := q.CreateTopic(ctx, dbq.CreateTopicParams{ID: "tp", UserID: "owner", Name: "billing"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateTopic(ctx, dbq.CreateTopicParams{ID: "tp2", UserID: "owner", Name: "billing"}); pgCode(err) != pgUniqueViolation {
		t.Errorf("duplicate topic name: %v", err)
	}
	byName, err := q.GetTopicByName(ctx, dbq.GetTopicByNameParams{UserID: "owner", Name: "billing"})
	if err != nil || byName.ID != tp.ID {
		t.Fatalf("by name = %+v, %v", byName, err)
	}
	tp, err = q.UpdateTopic(ctx, dbq.UpdateTopicParams{ID: "tp", StatusLine: "on track", Checkpoint: "cp"})
	if err != nil || tp.StatusLine != "on track" {
		t.Fatalf("update = %+v, %v", tp, err)
	}

	r, err := q.CreateTopicRecord(ctx, dbq.CreateTopicRecordParams{
		ID: "r1", TopicID: "tp", Kind: dbstate.RecordPending, Text: "ship it", SessionID: ptr("s1"),
	})
	if err != nil || r.Status != dbstate.RecordOpen {
		t.Fatalf("record = %+v, %v", r, err)
	}
	r, err = q.SetTopicRecordStatus(ctx, dbq.SetTopicRecordStatusParams{ID: "r1", Status: dbstate.RecordDone})
	if err != nil || r.Status != dbstate.RecordDone {
		t.Fatalf("record status = %+v, %v", r, err)
	}
	recs, err := q.ListTopicRecords(ctx, "tp")
	if err != nil || len(recs) != 1 {
		t.Fatalf("records = %d, %v", len(recs), err)
	}
	topics, err := q.ListTopics(ctx, "owner")
	if err != nil || len(topics) != 1 {
		t.Fatalf("topics = %d, %v", len(topics), err)
	}
	// Deleting a topic takes its records with it; deleting a session only detaches its records.
	if n, err := q.DeleteTopic(ctx, "tp"); err != nil || n != 1 {
		t.Fatalf("delete topic = %d, %v", n, err)
	}
	if recs, _ = q.ListTopicRecords(ctx, "tp"); len(recs) != 0 {
		t.Errorf("records survived their topic")
	}
}

// Every constant in dbstate must pass its CHECK, and a value outside the set must fail it.
func TestStateChecks(t *testing.T) {
	ctx := context.Background()
	q := dbq.New(testdb.New(t).Pool)
	newSession(t, q, "s1", nil)
	if _, err := q.CreateTopic(ctx, dbq.CreateTopicParams{ID: "tp", UserID: "owner", Name: "n"}); err != nil {
		t.Fatal(err)
	}

	t.Run("session status", func(t *testing.T) {
		for _, st := range []dbstate.SessionStatus{
			dbstate.SessionPending, dbstate.SessionActive, dbstate.SessionIdle, dbstate.SessionWaitingOnUser,
			dbstate.SessionSuperseded, dbstate.SessionCompleted, dbstate.SessionFailed, dbstate.SessionCancelled,
		} {
			if _, err := q.SetSessionStatus(ctx, dbq.SetSessionStatusParams{ID: "s1", Status: st}); err != nil {
				t.Errorf("%q rejected: %v", st, err)
			}
		}
		_, err := q.SetSessionStatus(ctx, dbq.SetSessionStatusParams{ID: "s1", Status: "implementing"})
		if pgCode(err) != pgCheckViolation {
			t.Errorf("legacy status accepted: %v", err)
		}
	})
	t.Run("merge policy", func(t *testing.T) {
		newProject(t, q, "p1", "a/b")
		for _, mp := range []dbstate.MergePolicy{dbstate.MergeAuto, dbstate.MergeApproval} {
			if _, err := q.SetProjectMergePolicy(ctx, dbq.SetProjectMergePolicyParams{ID: "p1", MergePolicy: mp}); err != nil {
				t.Errorf("%q rejected: %v", mp, err)
			}
		}
		_, err := q.SetProjectMergePolicy(ctx, dbq.SetProjectMergePolicyParams{ID: "p1", MergePolicy: "never"})
		if pgCode(err) != pgCheckViolation {
			t.Errorf("bad policy accepted: %v", err)
		}
	})
	t.Run("binding role and grant", func(t *testing.T) {
		for i, role := range []dbstate.BindingRole{dbstate.RoleMain, dbstate.RoleChild, dbstate.RoleAgent, dbstate.RoleSupervisor} {
			sid := "b" + string(rune('a'+i))
			newSession(t, q, sid, nil)
			grant := []dbstate.MCPGrantKind{dbstate.GrantNone, dbstate.GrantCoordination, dbstate.GrantWorkspace}[i%3]
			if _, err := q.CreateAgentBinding(ctx, dbq.CreateAgentBindingParams{SessionID: sid, Kind: "claude", Role: role, MCPGrantKind: grant}); err != nil {
				t.Errorf("role %q grant %q rejected: %v", role, grant, err)
			}
		}
		newSession(t, q, "bad", nil)
		for _, p := range []dbq.CreateAgentBindingParams{
			{SessionID: "bad", Kind: "claude", Role: "boss", MCPGrantKind: dbstate.GrantNone},
			{SessionID: "bad", Kind: "claude", Role: dbstate.RoleAgent, MCPGrantKind: "root"},
			{SessionID: "bad", Kind: "Claude Code", Role: dbstate.RoleAgent, MCPGrantKind: dbstate.GrantNone},
		} {
			if _, err := q.CreateAgentBinding(ctx, p); pgCode(err) != pgCheckViolation {
				t.Errorf("bad binding %+v accepted: %v", p, err)
			}
		}
	})
	t.Run("credential kind, state and hash", func(t *testing.T) {
		good := strings.Repeat("0a", 32)
		for i, k := range []dbstate.CredentialKind{dbstate.CredentialMCP, dbstate.CredentialGitRead, dbstate.CredentialGitPush} {
			_, err := q.CreateCredential(ctx, dbq.CreateCredentialParams{
				ID: string(k), Kind: k, BindingID: "x", TokenHash: strings.Repeat(string(rune('a'+i)), 64), SecretName: string(k),
			})
			if err != nil {
				t.Errorf("kind %q rejected: %v", k, err)
			}
		}
		for name, p := range map[string]dbq.CreateCredentialParams{
			"kind":           {ID: "k", Kind: "ssh", BindingID: "x", TokenHash: good, SecretName: "k"},
			"short hash":     {ID: "h1", Kind: dbstate.CredentialMCP, BindingID: "y", TokenHash: "abc", SecretName: "h1"},
			"plaintext hash": {ID: "h2", Kind: dbstate.CredentialMCP, BindingID: "y", TokenHash: "ml_" + strings.Repeat("a", 61), SecretName: "h2"},
			"upper-case hex": {ID: "h3", Kind: dbstate.CredentialMCP, BindingID: "y", TokenHash: strings.Repeat("AB", 32), SecretName: "h3"},
		} {
			if _, err := q.CreateCredential(ctx, p); pgCode(err) != pgCheckViolation {
				t.Errorf("%s: accepted: %v", name, err)
			}
		}
	})
	t.Run("credential state needs a Secret UID when published", func(t *testing.T) {
		_, err := q.MarkCredentialPublished(ctx, dbq.MarkCredentialPublishedParams{ID: string(dbstate.CredentialMCP)})
		if pgCode(err) != pgCheckViolation {
			t.Errorf("published without a UID: %v", err)
		}
	})
	t.Run("topic record", func(t *testing.T) {
		for _, k := range []dbstate.TopicRecordKind{dbstate.RecordNote, dbstate.RecordDecision, dbstate.RecordPending, dbstate.RecordReport} {
			if _, err := q.CreateTopicRecord(ctx, dbq.CreateTopicRecordParams{ID: string(k), TopicID: "tp", Kind: k, Text: "x"}); err != nil {
				t.Errorf("kind %q rejected: %v", k, err)
			}
		}
		if _, err := q.CreateTopicRecord(ctx, dbq.CreateTopicRecordParams{ID: "bad", TopicID: "tp", Kind: "rumor", Text: "x"}); pgCode(err) != pgCheckViolation {
			t.Errorf("bad kind accepted: %v", err)
		}
		_, err := q.SetTopicRecordStatus(ctx, dbq.SetTopicRecordStatusParams{ID: string(dbstate.RecordNote), Status: "later"})
		if pgCode(err) != pgCheckViolation {
			t.Errorf("bad status accepted: %v", err)
		}
	})
}

// Revoking a credential that never published may fill in the UID a resolver found by name.
func TestRevokePlannedCredentialRecordsObservedUID(t *testing.T) {
	ctx := context.Background()
	q := dbq.New(testdb.New(t).Pool)
	if _, err := q.CreateCredential(ctx, dbq.CreateCredentialParams{
		ID: "c1", Kind: dbstate.CredentialGitPush, BindingID: "s1", TokenHash: strings.Repeat("ab", 32), SecretName: "n1",
	}); err != nil {
		t.Fatal(err)
	}
	c, err := q.MarkCredentialRevoked(ctx, dbq.MarkCredentialRevokedParams{ID: "c1", SecretUID: ptr("observed")})
	if err != nil || c.SecretUID == nil || *c.SecretUID != "observed" {
		t.Fatalf("revoked planned = %+v, %v", c, err)
	}
}
