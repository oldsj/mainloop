package app_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/app"
	"github.com/oldsj/mainloop/server/internal/app/apicheck"
	"github.com/oldsj/mainloop/server/internal/app/apptest"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/testdb"
	"github.com/oldsj/mainloop/server/internal/transcripts"
)

// transcriptServer serves the real routes over a scratch database holding one session with one
// copied turn, from a kagent Session that is no longer followed (so no live tail is read).
func transcriptServer(t *testing.T) http.Handler {
	t.Helper()
	pool := testdb.New(t).Pool
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ('project-1', 'local-dev-user', 'o', 'r', 'o/r', 'https://x.example/o/r')`,
		`INSERT INTO sessions (id, user_id, project_id, title, description, prompt, status) VALUES ('session-1', 'local-dev-user', 'project-1', 'Fix the build', 'd', 'p', 'idle')`,
		`INSERT INTO sessions (id, user_id, title, description, prompt, status) VALUES ('session-x', 'someone-else', 'theirs', 'd', 'p', 'idle')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	q := dbq.New(pool)
	if err := q.InsertTranscriptSession(ctx, dbq.InsertTranscriptSessionParams{
		KagentSessionID: "ks-1", SessionID: "session-1", OwnerID: "local-dev-user", AgentNamespace: "agents", AgentName: "claude",
	}); err != nil {
		t.Fatal(err)
	}
	task := &a2a.Task{
		ID: "turn-1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
		History:   []*a2a.Message{a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("why does the orchestrator rollout fail"))},
		Artifacts: []*a2a.Artifact{{ID: "a1", Parts: []*a2a.Part{a2a.NewTextPart("the <b>rollout</b> waits on a probe")}}},
	}
	if _, err := transcripts.Save(ctx, q, transcripts.Copy{
		KagentSessionID: "ks-1", SessionID: "session-1", TaskID: "turn-1", TaskState: "completed",
		Entries: transcripts.Normalize(task, transcripts.DefaultLimits),
	}); err != nil {
		t.Fatal(err)
	}
	if err := q.EndTranscriptSession(ctx, dbq.EndTranscriptSessionParams{KagentSessionID: "ks-1", State: "complete"}); err != nil {
		t.Fatal(err)
	}
	deps := apptest.Deps(t, cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)), pool)
	deps.DB = up()
	h, err := app.New(deps).Handler()
	if err != nil {
		t.Fatal(err)
	}
	return apicheck.Wrap(t, h)
}

func TestTranscriptRoutes(t *testing.T) {
	t.Parallel()
	h := transcriptServer(t)

	r := send(h, "GET", "/sessions/session-1/transcript", "", nil)
	if r.status != 200 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	var tr api.SessionTranscript
	strict(t, r.body, &tr)
	if len(tr.Entries) != 2 || tr.HasMore || len(tr.Live) != 0 || tr.LiveError != nil ||
		len(tr.Sources) != 1 || tr.Sources[0].State != api.TranscriptSourceStateComplete {
		t.Fatalf("%s", r.body)
	}
	if r := send(h, "GET", "/sessions/session-x/transcript", "", nil); r.status != 404 {
		t.Fatalf("someone else's session: %d", r.status)
	}
	if r := send(h, "GET", "/sessions/session-1/transcript?limit=501", "", nil); r.status != 422 {
		t.Fatalf("limit past the bound: %d", r.status)
	}
	if r := send(h, "GET", "/sessions/session-1/transcript?after=1&latest=true", "", nil); r.status != 422 {
		t.Fatalf("after with latest: %d", r.status)
	}
	r = send(h, "GET", "/sessions/session-1/transcript?latest=true&limit=1", "", nil)
	var last api.SessionTranscript
	strict(t, r.body, &last)
	if r.status != 200 || len(last.Entries) != 1 || !last.HasEarlier || last.HasMore || *last.Entries[0].Id != *tr.Entries[1].Id {
		t.Fatalf("latest: %d %s", r.status, r.body)
	}

	r = send(h, "GET", "/transcripts/search?q=rollout&project_id=project-1", "", nil)
	if r.status != 200 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	var res api.TranscriptSearchResults
	strict(t, r.body, &res)
	if len(res.Hits) != 2 || res.Hits[0].SessionTitle != "Fix the build" {
		t.Fatalf("%s", r.body)
	}
	// Highlights come only from the search's own marks; tag-like text in an entry is never one.
	for _, h := range res.Hits {
		for _, p := range h.Snippet {
			if p.Match && p.Text != "rollout" {
				t.Fatalf("highlight %q", p.Text)
			}
		}
	}
	if r := send(h, "GET", "/transcripts/search", "", nil); r.status != 422 {
		t.Fatalf("no q: %d", r.status)
	}
}
