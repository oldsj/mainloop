package tasks_test

import (
	"sync"
	"testing"

	"github.com/oldsj/mainloop/server/internal/tasks"
)

// TestActiveCapacityQueuesRootTasks shows TASK_MAX_ACTIVE_GLOBAL counts every task the owner
// holds capacity for, root tasks included (the bug this cap fixes: CountGlobalCapacity only
// counted children). A task over the cap is admitted as queued, not refused.
func TestActiveCapacityQueuesRootTasks(t *testing.T) {
	r := newRig(t)
	r.svc.Limits.MaxActiveGlobal = 4
	for i := 0; i < 4; i++ {
		r.mustCreate(r.own, tasks.CreateInput{})
	}
	if n := r.count(`SELECT count(*) FROM tasks WHERE owner_id = $1`, owner); n != 4 {
		t.Fatalf("tasks before the 5th = %d", n)
	}

	fifth := r.mustCreate(r.own, tasks.CreateInput{})
	v := r.view(r.own, fifth)
	if v.Task.Status != "queued" || v.Task.Reason == nil || *v.Task.Reason != "capacity_queued" {
		t.Fatalf("5th task = %+v", v.Task)
	}
	if v.Task.QueuePosition == nil || *v.Task.QueuePosition != 0 {
		t.Fatalf("5th task queue position = %v, want 0", v.Task.QueuePosition)
	}
	if n := r.count(`SELECT count(*) FROM tasks WHERE owner_id = $1`, owner); n != 5 {
		t.Fatalf("tasks = %d, want 5 (queued, not refused)", n)
	}
}

// TestActiveCapacityStartsQueuedTaskWhenASlotFrees shows a queued task is picked up by the
// existing operation reconcile loop, not provisioned at create time, and only once a slot frees.
func TestActiveCapacityStartsQueuedTaskWhenASlotFrees(t *testing.T) {
	r := newRig(t)
	r.svc.Limits.MaxActiveGlobal = 4
	r.run()

	var ids []string
	for i := 0; i < 4; i++ {
		id := r.mustCreate(r.own, tasks.CreateInput{})
		r.eventually("provisioned", func() bool { return r.opState(id) == "completed" })
		ids = append(ids, id)
	}

	fifth := r.mustCreate(r.own, tasks.CreateInput{})
	v := r.view(r.own, fifth)
	if v.Task.Status != "queued" || v.Task.Reason == nil || *v.Task.Reason != "capacity_queued" {
		t.Fatalf("5th task before a slot frees = %+v", v.Task)
	}

	// A slot frees the way settlement does: the attempt is fenced and no longer holds capacity.
	att, _ := r.attemptOf(ids[0])
	r.exec(`UPDATE task_attempts SET state = 'fenced', stop_state = 'confirmed', capacity_held = false WHERE id = $1`, att)

	r.eventually("5th task provisioned", func() bool { return r.opState(fifth) == "completed" })
	v = r.view(r.own, fifth)
	if v.Task.Status != "running" || v.Task.Reason != nil {
		t.Fatalf("5th task after the slot freed = %+v", v.Task)
	}
}

// TestActiveCapacityFIFOOrder shows a later queued task never starts before an earlier one.
func TestActiveCapacityFIFOOrder(t *testing.T) {
	r := newRig(t)
	r.svc.Limits.MaxActiveGlobal = 1
	r.run()

	first := r.mustCreate(r.own, tasks.CreateInput{})
	r.eventually("first provisioned", func() bool { return r.opState(first) == "completed" })

	second := r.mustCreate(r.own, tasks.CreateInput{})
	third := r.mustCreate(r.own, tasks.CreateInput{})
	fourth := r.mustCreate(r.own, tasks.CreateInput{})

	pos := func(id string) *int { return r.view(r.own, id).Task.QueuePosition }
	if p := pos(second); p == nil || *p != 0 {
		t.Fatalf("2nd position = %v, want 0", p)
	}
	if p := pos(third); p == nil || *p != 1 {
		t.Fatalf("3rd position = %v, want 1", p)
	}
	if p := pos(fourth); p == nil || *p != 2 {
		t.Fatalf("4th position = %v, want 2", p)
	}

	// Free the one slot: only the oldest queued task (second) may take it.
	att, _ := r.attemptOf(first)
	r.exec(`UPDATE task_attempts SET state = 'fenced', stop_state = 'confirmed', capacity_held = false WHERE id = $1`, att)
	r.eventually("2nd provisioned", func() bool { return r.opState(second) == "completed" })

	tv, fv := r.view(r.own, third), r.view(r.own, fourth)
	if tv.Task.Status != "queued" || tv.Task.Reason == nil || *tv.Task.Reason != "capacity_queued" {
		t.Fatalf("3rd task jumped the queue: %+v", tv.Task)
	}
	if fv.Task.Status != "queued" || fv.Task.Reason == nil || *fv.Task.Reason != "capacity_queued" {
		t.Fatalf("4th task jumped the queue: %+v", fv.Task)
	}
	if p := pos(third); p == nil || *p != 0 {
		t.Fatalf("3rd position after promotion = %v, want 0", p)
	}
	if p := pos(fourth); p == nil || *p != 1 {
		t.Fatalf("4th position after promotion = %v, want 1", p)
	}
}

// TestActiveCapacityConcurrentAdmissionsNeverExceedTheCap shows the owner-wide advisory lock
// serialises concurrent creates: however many race to be admitted at once, exactly the cap ends up
// holding a slot and the rest are queued, never more than the cap.
func TestActiveCapacityConcurrentAdmissionsNeverExceedTheCap(t *testing.T) {
	r := newRig(t)
	const limit, n = 3, 10
	r.svc.Limits.MaxActiveGlobal = limit

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := r.create(r.own, tasks.CreateInput{})
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	active := r.count(`SELECT count(*) FROM tasks WHERE owner_id = $1 AND (reason IS NULL OR reason <> 'capacity_queued')`, owner)
	queued := r.count(`SELECT count(*) FROM tasks WHERE owner_id = $1 AND reason = 'capacity_queued'`, owner)
	if active != limit || queued != n-limit {
		t.Fatalf("active = %d queued = %d, want %d/%d", active, queued, limit, n-limit)
	}
}
