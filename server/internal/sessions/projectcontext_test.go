package sessions_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/ownermemory"
	"github.com/oldsj/mainloop/server/internal/projectcontext"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

// taskSession makes a native session the attempt of a task in project p1, briefed with no
// instructions and no memory, and returns it.
func (e *env) taskSession() string {
	e.t.Helper()
	ctx := context.Background()
	sid := e.newSession("the brief")
	e.waitState(e.firstDelivery(sid), dbstate.DeliveryCompleted)
	for _, q := range []string{
		`INSERT INTO projects (id,user_id,owner,name,full_name,html_url) VALUES ('p1',$1,'o','n','o/n','https://github.com/o/n')`,
		`INSERT INTO tasks (id, owner_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source, status, project_id)
		    VALUES ('t1', $1, 't1', 'Ship it', 'brief', 'coordination', 'p', 'explicit', 'running', 'p1')`,
	} {
		if _, err := e.pool.Exec(ctx, q, owner); err != nil {
			e.t.Fatal(err)
		}
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO task_attempts (id, task_id, number, profile_id, native_provider, configuration_revision,
		agent_namespace, agent_name, role, depth, state, session_id, instructions_version, memory_seq)
		VALUES ('a1', 't1', 1, 'p', 'claude', 'r', 'ns', 'ag', 'supervisor', 1, 'active', $1, 0, 0)`, sid); err != nil {
		e.t.Fatal(err)
	}
	return sid
}

func (e *env) sendTo(sid, text string) string {
	e.t.Helper()
	id := uuid.NewString()
	if _, err := e.svc.SendToAs(context.Background(), sessions.SourceMainloop, sid, id, text); err != nil {
		e.t.Fatalf("send: %v", err)
	}
	return id
}

// TestARunningTaskGetsProjectContextChangesOnItsNextMessage: a change to the project's instructions
// or memory after a task's brief reaches the task's session in front of its next message, once, and
// only what changed; a message kagent never saw gives the change back for the next one.
func TestARunningTaskGetsProjectContextChangesOnItsNextMessage(t *testing.T) {
	var log sentLog
	e := newEnv(t, carryOpt(t, 6, &log))
	e.withProviders()
	e.run()
	ctx := context.Background()
	sid := e.taskSession()
	pc := &projectcontext.Service{Pool: e.pool, OwnerID: owner}
	main := projectcontext.Recorder{Author: projectcontext.AuthorMain}

	if d := e.waitState(e.sendTo(sid, "unchanged"), dbstate.DeliveryCompleted); d.SentText != nil {
		t.Fatalf("nothing changed, yet the message was rewritten: %q", *d.SentText)
	}

	if _, err := pc.SetInstructions(ctx, "p1", "Merge with --squash.", 0); err != nil {
		t.Fatal(err)
	}
	entry, err := pc.Remember(ctx, "p1", main, "The owner reviews UI on a phone.", "")
	if err != nil {
		t.Fatal(err)
	}
	id := e.sendTo(sid, "keep going")
	e.waitState(id, dbstate.DeliveryCompleted)
	want := projectcontext.UpdateMarker + " Your context changed since you were given it. Where they differ, this replaces what you were given.\n" +
		"## Project instructions (owner, v1, were none)\nMerge with --squash.\n" +
		"## Project memory changes\n- New: The owner reviews UI on a phone. (memory " + entry.ID + ")\n" +
		newMarker + "\nkeep going"
	if got := log.last(t); got.ID != id || got.Text != want {
		t.Fatalf("sent %q\nwant %q", got.Text, want)
	}
	if m := e.lastUserMessage(sid); m != "keep going" {
		t.Fatalf("the mirrored message is %q, want what was written", m)
	}

	if d := e.waitState(e.sendTo(sid, "and again"), dbstate.DeliveryCompleted); d.SentText != nil {
		t.Fatalf("a change was given twice: %q", *d.SentText)
	}

	// Only the removal goes next; a message kagent refused gives it back.
	if _, err := pc.Forget(ctx, "p1", main, entry.ID); err != nil {
		t.Fatal(err)
	}
	e.srv.FailNext("SendStreamingMessage", kagenttest.Fault{RPC: &kagenttest.RPCError{Code: -32602, Message: "invalid params"}})
	lost := e.sendTo(sid, "lost")
	e.waitState(lost, dbstate.DeliveryFailed)
	id = e.sendTo(sid, "after the refusal")
	e.waitState(id, dbstate.DeliveryCompleted)
	want = projectcontext.UpdateMarker + " Your context changed since you were given it. Where they differ, this replaces what you were given.\n" +
		"## Project memory changes\n- Removed: The owner reviews UI on a phone. (memory " + entry.ID + ")\n" +
		newMarker + "\nafter the refusal"
	if got := log.last(t); got.ID != id || got.Text != want {
		t.Fatalf("sent %q\nwant %q", got.Text, want)
	}
	for _, m := range log.all() {
		if m.ID != id && m.ID != lost && strings.Contains(m.Text, "- Removed:") {
			t.Fatalf("the removal went out with %s too", m.ID)
		}
	}
}

// TestAMainSessionGetsNoProjectContext: the main session has no project, so its messages carry none.
func TestAMainSessionGetsNoProjectContext(t *testing.T) {
	var log sentLog
	e := newEnv(t, carryOpt(t, 6, &log))
	e.withProviders()
	e.run()
	e.taskSession()
	pc := &projectcontext.Service{Pool: e.pool, OwnerID: owner}
	if _, err := pc.SetInstructions(context.Background(), "p1", "Merge with --squash.", 0); err != nil {
		t.Fatal(err)
	}
	e.waitState(e.send("hello"), dbstate.DeliveryCompleted)
	if got := log.last(t).Text; strings.Contains(got, "## Project instructions") || strings.Contains(got, "Merge with --squash.") {
		t.Fatalf("main got project context: %q", got)
	}
}

// TestRunningSessionsGetOwnerMemoryChangesOnce: a change to owner memory reaches a running task
// session and the main session in front of each one's next message, once: the whole section for
// a session that had none, then only what changed.
func TestRunningSessionsGetOwnerMemoryChangesOnce(t *testing.T) {
	var log sentLog
	e := newEnv(t, carryOpt(t, 6, &log))
	e.withProviders()
	e.run()
	ctx := context.Background()
	sid := e.taskSession()
	om := &ownermemory.Service{Pool: e.pool, OwnerID: owner}
	main := ownermemory.Recorder{Author: ownermemory.AuthorMain}

	// The main session's context block carries the owner's memory as it is when rendered.
	first, err := om.Remember(ctx, main, "I review UI on my phone.", "")
	if err != nil {
		t.Fatal(err)
	}
	e.waitState(e.send("hello"), dbstate.DeliveryCompleted)
	if got := log.last(t).Text; !strings.Contains(got, "## Owner memory\n") || !strings.Contains(got, "- I review UI on my phone. (owner memory "+first.ID+", confirmed ") {
		t.Fatalf("main's block = %q", got)
	}
	if d := e.waitState(e.send("again"), dbstate.DeliveryCompleted); d.SentText != nil && strings.Contains(*d.SentText, "Owner memory") {
		t.Fatalf("main was given owner memory twice: %q", *d.SentText)
	}

	// The task session had none, so it gets the whole section once.
	id := e.sendTo(sid, "keep going")
	e.waitState(id, dbstate.DeliveryCompleted)
	if got := log.last(t); got.ID != id || !strings.HasPrefix(got.Text, projectcontext.UpdateMarker+" Your context changed") ||
		!strings.Contains(got.Text, "## Owner memory\n") || !strings.HasSuffix(got.Text, newMarker+"\nkeep going") {
		t.Fatalf("task sent %q", got.Text)
	}
	if d := e.waitState(e.sendTo(sid, "and again"), dbstate.DeliveryCompleted); d.SentText != nil {
		t.Fatalf("a change was given twice: %q", *d.SentText)
	}

	// A later entry goes to both as a change, once.
	second, err := om.Remember(ctx, main, "Prefer short PRs.", "")
	if err != nil {
		t.Fatal(err)
	}
	want := "## Owner memory changes\n- New: Prefer short PRs. (owner memory " + second.ID + ", confirmed "
	id = e.sendTo(sid, "next")
	e.waitState(id, dbstate.DeliveryCompleted)
	if got := log.last(t); got.ID != id || !strings.Contains(got.Text, want) || strings.Contains(got.Text, "## Owner memory\n") {
		t.Fatalf("task sent %q", got.Text)
	}
	id = e.send("main next")
	e.waitState(id, dbstate.DeliveryCompleted)
	if got := log.last(t); got.ID != id || !strings.Contains(got.Text, want) {
		t.Fatalf("main sent %q", got.Text)
	}
	if d := e.waitState(e.send("main again"), dbstate.DeliveryCompleted); d.SentText != nil && strings.Contains(*d.SentText, "Owner memory") {
		t.Fatalf("main was given the change twice: %q", *d.SentText)
	}
}

// lastUserMessage is the newest owner-side message mirrored in the session's conversation.
func (e *env) lastUserMessage(sid string) string {
	e.t.Helper()
	var s string
	if err := e.pool.QueryRow(context.Background(), `SELECT content FROM session_messages WHERE session_id = $1 AND role = 'user'
		ORDER BY created_at DESC, id DESC LIMIT 1`, sid).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}
