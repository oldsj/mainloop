package tasks_test

import (
	"context"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

func (e *env) seenAPI() *tasks.TasksAPI { return &tasks.TasksAPI{Svc: e.svc, OwnerID: owner} }

func (e *env) getSeen() api.Seen {
	e.t.Helper()
	r, err := e.seenAPI().GetSeen(context.Background(), api.GetSeenRequestObject{})
	if err != nil {
		e.t.Fatal(err)
	}
	return api.Seen(r.(api.GetSeen200JSONResponse))
}

func (e *env) visit() api.Seen {
	e.t.Helper()
	r, err := e.seenAPI().RecordVisit(context.Background(), api.RecordVisitRequestObject{})
	if err != nil {
		e.t.Fatal(err)
	}
	return api.Seen(r.(api.RecordVisit200JSONResponse))
}

func (e *env) markSeen(id string) (api.SeenTask, error) {
	r, err := e.seenAPI().MarkTaskSeen(context.Background(), api.MarkTaskSeenRequestObject{TaskId: id})
	if err != nil {
		return api.SeenTask{}, err
	}
	return api.SeenTask(r.(api.MarkTaskSeen200JSONResponse)), nil
}

// With no record nothing is unseen; the first visit has no previous one, the next one's previous
// is the first, whichever device made it.
func TestSeenVisitsCarryAcrossCalls(t *testing.T) {
	e := newEnv(t)
	before := time.Now().Add(-time.Second)
	empty := e.getSeen()
	if empty.Since.Before(before) || empty.LastVisit != nil || empty.PreviousVisit != nil || len(empty.Tasks) != 0 {
		t.Fatalf("empty state = %+v", empty)
	}
	first := e.visit()
	if first.LastVisit == nil || first.PreviousVisit != nil {
		t.Fatalf("first visit = %+v", first)
	}
	second := e.visit()
	if second.PreviousVisit == nil || !second.PreviousVisit.Equal(*first.LastVisit) || second.LastVisit.Before(*first.LastVisit) {
		t.Fatalf("second visit = %+v, first %+v", second, first)
	}
	if !second.Since.Equal(first.Since) {
		t.Fatalf("since moved: %v then %v", first.Since, second.Since)
	}
	if got := e.getSeen(); !got.LastVisit.Equal(*second.LastVisit) || !got.PreviousVisit.Equal(*first.LastVisit) {
		t.Fatalf("read after visits = %+v", got)
	}
}

func TestMarkTaskSeenIsOwnTasksLaterWinsAndAgesOut(t *testing.T) {
	e := newEnv(t)
	id := e.mustCreate(e.own, tasks.CreateInput{})
	if _, err := e.markSeen("nope"); apierr.StatusOf(err) != 404 {
		t.Fatalf("unknown task: %v", err)
	}
	e.exec(`INSERT INTO tasks (id, owner_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source, status)
		SELECT 'foreign', 'someone-else', 'foreign', title, brief, mode, assigned_profile_id, selection_source, status FROM tasks WHERE id = $1`, id)
	if _, err := e.markSeen("foreign"); apierr.StatusOf(err) != 404 {
		t.Fatalf("another owner's task: %v", err)
	}

	m, err := e.markSeen(id)
	if err != nil {
		t.Fatal(err)
	}
	var updated time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT updated_at FROM tasks WHERE id = $1`, id).Scan(&updated); err != nil {
		t.Fatal(err)
	}
	if m.TaskId != id || m.SeenAt.Before(updated) {
		t.Fatalf("mark = %+v, task updated %v", m, updated)
	}
	// An earlier mark never replaces a later one.
	e.exec(`UPDATE task_seen SET seen_at = now() + interval '1 hour' WHERE task_id = $1`, id)
	if again, err := e.markSeen(id); err != nil || !again.SeenAt.After(m.SeenAt.Add(30*time.Minute)) {
		t.Fatalf("a later mark was replaced: %+v %v", again, err)
	}
	got := e.getSeen()
	if len(got.Tasks) != 1 || got.Tasks[0].TaskId != id {
		t.Fatalf("seen tasks = %+v", got.Tasks)
	}

	// Past SeenKeep a mark is neither listed nor kept.
	e.exec(`UPDATE task_seen SET seen_at = now() - interval '61 days' WHERE task_id = $1`, id)
	if got := e.getSeen(); len(got.Tasks) != 0 {
		t.Fatalf("an aged-out mark is listed: %+v", got.Tasks)
	}
	other := e.mustCreate(e.own, tasks.CreateInput{})
	if _, err := e.markSeen(other); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM task_seen WHERE task_id = $1`, id); n != 0 {
		t.Fatalf("the aged-out mark was kept")
	}
}
