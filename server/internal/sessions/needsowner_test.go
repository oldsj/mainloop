package sessions_test

import (
	"context"
	"testing"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/attention"
)

// A session reads needs_owner exactly when the attention visibility rule says it has a visible
// item: on the list (one batched read), on the detail and on the conversation.
func TestSessionsReportNeedsOwnerFromTheAttentionRule(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	ctx := context.Background()
	a := e.API()
	needy, quiet, archived := e.newSession("one"), e.newSession("two"), e.newSession("three")
	for _, id := range []string{needy, archived} {
		openItem(t, e, id)
	}
	// An archived session's item is hidden: the rule, not the row, decides.
	if _, err := e.pool.Exec(ctx, `UPDATE sessions SET archived_at = now() WHERE id = $1`, archived); err != nil {
		t.Fatal(err)
	}

	list, err := a.ListSessions(ctx, api.ListSessionsRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range list.(api.ListSessions200JSONResponse) {
		got[s.Id] = s.NeedsOwner
	}
	if !got[needy] || got[quiet] {
		t.Errorf("list needs_owner = %v", got)
	}

	for id, want := range map[string]bool{needy: true, quiet: false, archived: false} {
		if snapNeeds := (&attention.Service{Pool: e.pool, OwnerID: owner}).NeedsOwner(ctx, id); snapNeeds != want {
			t.Fatalf("attention.NeedsOwner(%s) = %v, test setup is wrong", id, snapNeeds)
		}
		one, err := a.GetSession(ctx, api.GetSessionRequestObject{SessionId: id})
		if err != nil {
			t.Fatal(err)
		}
		if s := api.Session(one.(api.GetSession200JSONResponse)); s.NeedsOwner != want {
			t.Errorf("GetSession(%s).needs_owner = %v, want %v", id, s.NeedsOwner, want)
		}
		conv, err := a.GetSessionConversation(ctx, api.GetSessionConversationRequestObject{SessionId: id})
		if err != nil {
			t.Fatal(err)
		}
		if s := conv.(api.GetSessionConversation200JSONResponse).Session; s.NeedsOwner != want {
			t.Errorf("conversation(%s).needs_owner = %v, want %v", id, s.NeedsOwner, want)
		}
	}
}

// openItem opens a queue item about the session, as another slice would.
func openItem(t *testing.T, e *env, sessionID string) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := attention.Open(ctx, tx, owner, attention.NewQueueItem{ID: "q-" + sessionID, Title: "Question", SessionID: &sessionID}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
