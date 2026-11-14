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

// TestBriefCarriesTheProjectMemoryPinnedOnTheAttempt: a brief carries the project's active memory
// after the instructions, as it stood when the attempt first got its brief. Proposals are not
// carried, and a resend renders the pinned entries even after one is forgotten.
func TestBriefCarriesTheProjectMemoryPinnedOnTheAttempt(t *testing.T) {
	r := newRig(t)
	r.run()
	ctx := context.Background()
	pc := &projectcontext.Service{Pool: r.pool, OwnerID: owner}
	main := projectcontext.Recorder{Author: projectcontext.AuthorMain}
	worker := projectcontext.Recorder{Author: projectcontext.AuthorTask}

	if _, err := pc.SetInstructions(ctx, "p1", "UI demos use fictional data.", 0); err != nil {
		t.Fatal(err)
	}
	kept, err := pc.Remember(ctx, "p1", main, "The owner reviews UI on a phone.", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pc.Remember(ctx, "p1", worker, "Unreviewed proposal.", ""); err != nil {
		t.Fatal(err)
	}
	first := r.mustCreate(r.own, tasks.CreateInput{ProjectID: "p1", Brief: "Ship the thing"})
	r.eventually("supervisor provisioned", func() bool { return r.opState(first) == "completed" })
	attempt, session := r.attemptOf(first)
	msg := r.str(`SELECT content FROM session_messages WHERE session_id = $1`, session)
	instr, mem := strings.Index(msg, "## Project instructions"), strings.Index(msg, "## Project memory\n")
	if instr < 0 || mem < instr || mem > strings.Index(msg, strings.TrimSpace(agentcontext.Guidance)) ||
		!strings.Contains(msg, "- The owner reviews UI on a phone. (memory "+kept.ID+")") || strings.Contains(msg, "Unreviewed proposal.") {
		t.Fatalf("brief = %q", msg)
	}
	if n := r.count(`SELECT memory_seq FROM task_attempts WHERE id = $1`, attempt); n != 1 {
		t.Fatalf("pinned memory sequence = %d, want 1", n)
	}

	if _, err := pc.Forget(ctx, "p1", main, kept.ID); err != nil {
		t.Fatal(err)
	}
	again, err := projectcontext.MemoryForAttempt(ctx, dbq.New(r.pool), attempt, "p1")
	if err != nil || len(again) != 1 || again[0].ID != kept.ID {
		t.Fatalf("resend memory = %+v, %v", again, err)
	}
	second := r.mustCreate(r.own, tasks.CreateInput{ProjectID: "p1", Brief: "Ship another"})
	r.eventually("second supervisor provisioned", func() bool { return r.opState(second) == "completed" })
	_, secondSession := r.attemptOf(second)
	if msg := r.str(`SELECT content FROM session_messages WHERE session_id = $1`, secondSession); strings.Contains(msg, "## Project memory") {
		t.Fatalf("a forgotten entry reached a later brief: %q", msg)
	}
}
