package transcripts

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

const (
	owner   = "owner-1"
	session = "sess-1"
	kagent  = "ks-1"
)

func setup(t *testing.T) (*testdb.DB, *dbq.Queries) {
	t.Helper()
	d := testdb.New(t)
	ctx := context.Background()
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO sessions (id, user_id, title, description, prompt) VALUES ($1, $2, 't', '', '')`, session, owner); err != nil {
		t.Fatal(err)
	}
	q := dbq.New(d.Pool)
	if err := q.InsertTranscriptSession(ctx, dbq.InsertTranscriptSessionParams{
		KagentSessionID: kagent, SessionID: session, OwnerID: owner, AgentNamespace: "agents", AgentName: "claude",
	}); err != nil {
		t.Fatal(err)
	}
	return d, q
}

// secretTurn is a turn whose tool result prints fictional credentials next to ordinary words.
func secretTurn() *a2a.Task {
	out := "deploying orchestrator\nAPI_TOKEN=zebracornflakes\nAuthorization: Bearer quokkatrampoline\nrollout finished"
	return &a2a.Task{
		ID: "task-secret", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
		Artifacts: []*a2a.Artifact{
			artifact("c", 10, toolCall("c1", "Bash", map[string]any{"command": "make deploy"})),
			artifact("r", 20, toolResult("c1", "Bash", out, false)),
		},
	}
}

func save(t *testing.T, q *dbq.Queries, task *a2a.Task) bool {
	t.Helper()
	ok, err := Save(context.Background(), q, Copy{
		KagentSessionID: kagent, SessionID: session, TaskID: string(task.ID), TaskState: string(task.Status.State),
		Entries: Normalize(task, DefaultLimits),
	})
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func search(t *testing.T, q *dbq.Queries, query string) []dbq.SearchTranscriptsRow {
	t.Helper()
	rows, err := q.SearchTranscripts(context.Background(), dbq.SearchTranscriptsParams{Query: query, OwnerID: owner, MaxResults: 10})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestSaveIsIdempotent(t *testing.T) {
	d, q := setup(t)
	if !save(t, q, secretTurn()) {
		t.Fatal("first copy not written")
	}
	if save(t, q, secretTurn()) {
		t.Fatal("second copy written")
	}
	var entries, bytes int64
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM transcript_entries), (SELECT stored_bytes FROM transcript_sessions)`).Scan(&entries, &bytes); err != nil {
		t.Fatal(err)
	}
	if entries != 2 || bytes == 0 {
		t.Fatalf("entries=%d stored_bytes=%d", entries, bytes)
	}
}

// Redaction runs before the insert, so neither the stored text nor the generated search vector
// holds a redacted credential, and searching for one finds nothing.
func TestRedactedSecretsAreNotSearchable(t *testing.T) {
	d, q := setup(t)
	save(t, q, secretTurn())
	ctx := context.Background()

	for _, secret := range []string{"zebracornflakes", "quokkatrampoline"} {
		if rows := search(t, q, secret); len(rows) != 0 {
			t.Errorf("search %q found %+v", secret, rows)
		}
		var n int
		if err := d.Pool.QueryRow(ctx,
			`SELECT count(*) FROM transcript_entries WHERE text ILIKE '%' || $1 || '%' OR tsv::text ILIKE '%' || $1 || '%'`, secret).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%q stored in %d entries", secret, n)
		}
	}
	rows := search(t, q, "orchestrator rollout")
	if len(rows) != 1 || rows[0].Kind != string(KindToolResult) || rows[0].SessionID != session || !strings.Contains(rows[0].Snippet, "orchestrator") {
		t.Fatalf("ordinary words: %+v", rows)
	}
	if rows := search(t, q, "make deploy"); len(rows) != 1 || rows[0].Kind != string(KindToolCall) {
		t.Fatalf("tool call: %+v", rows)
	}
}

func TestSearchIsOwnerScoped(t *testing.T) {
	_, q := setup(t)
	save(t, q, secretTurn())
	rows, err := q.SearchTranscripts(context.Background(), dbq.SearchTranscriptsParams{Query: "orchestrator", OwnerID: "someone-else", MaxResults: 10})
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

func TestSessionDeleteRemovesTranscript(t *testing.T) {
	d, q := setup(t)
	save(t, q, secretTurn())
	ctx := context.Background()
	if _, err := d.Pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, session); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.Pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM transcript_entries) + (SELECT count(*) FROM transcript_tasks) + (SELECT count(*) FROM transcript_sessions)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d rows left", n)
	}
}
