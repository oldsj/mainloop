package tasks_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// ownerTask is the owner's POST /tasks in p1: a project task that runs under the standing supervisor.
func ownerTask(req, title string) tasks.CreateInput {
	return tasks.CreateInput{RequestID: req, Title: title, Brief: "look into it", ProjectID: "p1", UnderSupervisor: true}
}

func TestOwnersProjectTaskRunsUnderTheStandingSupervisor(t *testing.T) {
	r := newRig(t)
	r.run()
	ctx := context.Background()

	// Given a project with no standing supervisor
	// When the owner starts a task in it
	child := r.mustCreate(r.own, ownerTask("o-1", "Read the flaky test"))

	// Then the project's standing supervisor is started, and the task is its child
	sup := r.str(`SELECT id FROM tasks WHERE project_id = 'p1' AND standing`)
	task := r.view(r.own, child).Task
	if task.ParentTaskId == nil || *task.ParentTaskId != sup || task.RootTaskId != sup || task.CreatorBindingId != nil {
		t.Fatalf("owner task = %+v, want a child of %s", task, sup)
	}

	// And the supervisor is told, in its session, once it has its brief
	r.eventually("supervisor told of the owner's task", func() bool {
		r.finishTurns()
		return r.count(`SELECT count(*) FROM session_messages m JOIN task_attempts a ON a.session_id = m.session_id
			WHERE a.task_id = $1 AND m.content LIKE '%the owner started child task ' || $2 || ' under you%'
			AND m.content LIKE '%Read the flaky test%'`, sup, child) == 1
	})

	_, session := r.attemptOf(sup)
	if first := r.firstMessage(session); strings.Contains(first, "the owner started") {
		t.Fatalf("the notice came before the supervisor's brief:\n%s", first)
	}

	// And a retry of the same request finds the same task and tells the supervisor nothing new
	if again := r.mustCreate(r.own, ownerTask("o-1", "Read the flaky test")); again != child {
		t.Fatalf("replay made task %s, want %s", again, child)
	}
	if n := r.count(`SELECT count(*) FROM task_event_deliveries WHERE notice_task_id IS NOT NULL`); n != 1 {
		t.Fatalf("notices = %d, want 1", n)
	}

	// And the owner's next project task joins the same supervisor
	next := r.mustCreate(r.own, ownerTask("o-2", "Bump the linter"))
	if p := r.view(r.own, next).Task.ParentTaskId; p == nil || *p != sup {
		t.Fatalf("second owner task parent = %v", p)
	}
	if n := r.count(`SELECT count(*) FROM tasks WHERE project_id = 'p1' AND standing`); n != 1 {
		t.Fatalf("standing supervisors = %d", n)
	}

	// And the owner still manages it as any task
	if _, err := r.svc.Get(ctx, r.own, next); err != nil {
		t.Fatal(err)
	}
}

func TestOwnersProjectTasksCountAgainstTheProjectLimit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	one := 1
	if _, err := e.svc.SetWorkerLimit(ctx, owner, "p1", &one, tasks.LimitChanger{}); err != nil {
		t.Fatal(err)
	}

	// Given a project limited to one worker, holding one owner task
	e.mustCreate(e.own, ownerTask("o-1", "first"))

	// When the owner starts another, Then it is refused as the supervisor's own delegate would be
	_, err := e.create(e.own, ownerTask("o-2", "second"))
	wantRefusal(t, err, 409, tasks.ReasonProjectCapacity)
	if n := e.count(`SELECT count(*) FROM task_event_deliveries WHERE notice_task_id IS NOT NULL`); n != 1 {
		t.Fatalf("notices = %d: a refused task told the supervisor", n)
	}
}

func TestOwnerTasksWithoutAProjectStayRoots(t *testing.T) {
	e := newEnv(t)

	// Given an owner task with no project, Then it is a root, and no supervisor is started
	id := e.mustCreate(e.own, tasks.CreateInput{RequestID: "o-1", UnderSupervisor: true})
	if v := e.view(e.own, id).Task; v.ParentTaskId != nil {
		t.Fatalf("task = %+v", v)
	}
	if n := e.count(`SELECT count(*) FROM tasks WHERE standing`); n != 0 {
		t.Fatalf("standing tasks = %d", n)
	}

	// And a foreign project is refused before a supervisor is started
	in := ownerTask("o-2", "x")
	in.ProjectID = "foreign"
	_, err := e.create(e.own, in)
	wantRefusal(t, err, 404, tasks.ReasonProjectNotFound)

	// And main's project task is still refused: it sends work with supervisor_send
	main := tasks.Principal{OwnerID: owner, Role: dbstate.RoleMain, BindingID: "main-1"}
	_, err = e.create(main, ownerTask("m-1", "x"))
	wantRefusal(t, err, 409, tasks.ReasonProjectSupervisor)
}

func TestOwnerTaskNoticeWaitsForTheSupervisorsBrief(t *testing.T) {
	// Given a project with no standing supervisor, whose first brief is held up on its way to the
	// supervisor's session after the attempt is already active
	r := newRig(t)
	gate := make(chan struct{})
	var open sync.Once
	release := func() { open.Do(func() { close(gate) }) }
	t.Cleanup(release)
	r.ses.hold, r.ses.holdIf = gate, func(text string) bool { return !strings.Contains(text, "the owner started") }
	r.run()

	// When the owner starts a task in it
	child := r.mustCreate(r.own, ownerTask("o-1", "Read the flaky test"))
	sup := r.str(`SELECT id FROM tasks WHERE project_id = 'p1' AND standing`)
	r.eventually("supervisor active, brief not yet recorded", func() bool {
		return r.count(`SELECT count(*) FROM task_attempts a JOIN tasks t ON t.current_attempt_id = a.id
			WHERE t.id = $1 AND a.state = 'active' AND a.session_id IS NOT NULL AND a.brief_delivery_id IS NULL`, sup) == 1
	})
	r.eventually("notice looked at while the brief is held", func() bool {
		return r.count(`SELECT count(*) FROM task_event_deliveries WHERE notice_task_id = $1 AND attempts >= 3`, child) == 1
	})

	// Then the notice waits: nothing reaches the supervisor before its brief
	if n := r.count(`SELECT count(*) FROM task_event_deliveries WHERE notice_task_id = $1 AND delivery_id IS NOT NULL`, child); n != 0 {
		t.Fatal("the notice was sent before the supervisor's brief")
	}

	// And once the brief is recorded, the notice follows it
	release()
	_, session := r.attemptOf(sup)
	r.eventually("supervisor told of the owner's task", func() bool {
		r.finishTurns()
		return r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1 AND content LIKE '%the owner started child task%'`, session) == 1
	})
	if first := r.firstMessage(session); strings.Contains(first, "the owner started") {
		t.Fatalf("the notice came before the supervisor's brief:\n%s", first)
	}
}
