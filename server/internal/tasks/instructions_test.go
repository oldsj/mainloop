package tasks_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/agentcontext"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/projectcontext"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// TestBriefCarriesTheProjectInstructionsPinnedOnTheAttempt: a task's brief carries its project's
// instructions at the version current when the attempt first got its brief, recorded on the
// attempt. A later edit reaches the next attempt, not one already briefed, and a resend of the
// first attempt's brief renders the version it was pinned to.
func TestBriefCarriesTheProjectInstructionsPinnedOnTheAttempt(t *testing.T) {
	r := newRig(t)
	r.run()
	ctx := context.Background()
	pc := &projectcontext.Service{Pool: r.pool, OwnerID: owner}

	bare := r.mustCreate(r.own, tasks.CreateInput{ProjectID: "p1", Brief: "Before any instructions"})
	r.eventually("bare supervisor provisioned", func() bool { return r.opState(bare) == "completed" })
	bareAttempt, bareSession := r.attemptOf(bare)
	if msg := r.str(`SELECT content FROM session_messages WHERE session_id = $1`, bareSession); strings.Contains(msg, "Project instructions") {
		t.Fatalf("a project without instructions got a section: %q", msg)
	}
	if n := r.count(`SELECT instructions_version FROM task_attempts WHERE id = $1`, bareAttempt); n != 0 {
		t.Fatalf("pinned version without instructions = %d, want 0", n)
	}

	if _, err := pc.SetInstructions(ctx, "p1", "UI demos use fictional data.", 0); err != nil {
		t.Fatal(err)
	}
	first := r.mustCreate(r.own, tasks.CreateInput{ProjectID: "p1", Brief: "Ship the thing"})
	r.eventually("first supervisor provisioned", func() bool { return r.opState(first) == "completed" })
	firstAttempt, firstSession := r.attemptOf(first)
	msg := r.str(`SELECT content FROM session_messages WHERE session_id = $1`, firstSession)
	section := "## Project instructions (owner, v1)\n"
	if !strings.Contains(msg, section) || !strings.Contains(msg, "UI demos use fictional data.") ||
		strings.Index(msg, section) > strings.Index(msg, strings.TrimSpace(agentcontext.Guidance)) {
		t.Fatalf("brief = %q", msg)
	}
	if n := r.count(`SELECT instructions_version FROM task_attempts WHERE id = $1`, firstAttempt); n != 1 {
		t.Fatalf("pinned version = %d, want 1", n)
	}

	if _, err := pc.SetInstructions(ctx, "p1", "Merge with --squash.", 1); err != nil {
		t.Fatal(err)
	}
	second := r.mustCreate(r.own, tasks.CreateInput{ProjectID: "p1", Brief: "Ship another"})
	r.eventually("second supervisor provisioned", func() bool { return r.opState(second) == "completed" })
	_, secondSession := r.attemptOf(second)
	if msg := r.str(`SELECT content FROM session_messages WHERE session_id = $1`, secondSession); !strings.Contains(msg, "(owner, v2)") || !strings.Contains(msg, "Merge with --squash.") {
		t.Fatalf("second brief = %q", msg)
	}

	// A resend of the first attempt's brief renders the version it was pinned to.
	again, err := projectcontext.ForAttempt(ctx, dbq.New(r.pool), firstAttempt, "p1")
	if err != nil || again.Version != 1 || again.Text != "UI demos use fictional data." {
		t.Fatalf("resend instructions = %+v, %v", again, err)
	}
	// The attempt briefed before any instructions keeps none.
	if none, err := projectcontext.ForAttempt(ctx, dbq.New(r.pool), bareAttempt, "p1"); err != nil || none.Version != 0 || projectcontext.Section(none) != "" {
		t.Fatalf("bare resend instructions = %+v, %v", none, err)
	}
}
