package tasks_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// liveTask creates a running coordination task with one active attempt whose binding names
// kagentSessionID, current on the task, as the liveness sweep requires. The binding is a real
// agent_bindings row, resolved the same way KagentLiveness resolves one in production, so a test
// keys the fake kagent client by the kagent id, never Mainloop's own session/binding id. An empty
// kagentSessionID leaves the binding's kagent_session_id NULL, as a coordination attempt has
// before its brief is delivered.
func (r *rig) liveTask(kagentSessionID string) (taskID, bindingID string) {
	r.t.Helper()
	taskID = r.mustCreate(r.own, tasks.CreateInput{})
	bindingID = uuid.NewString()
	ctx := context.Background()
	q := dbq.New(r.pool)
	if _, err := q.CreateSession(ctx, dbq.CreateSessionParams{ID: bindingID, UserID: owner, Title: "live"}); err != nil {
		r.t.Fatal(err)
	}
	if _, err := q.CreateAgentBinding(ctx, dbq.CreateAgentBindingParams{
		SessionID: bindingID, Kind: "claude", Role: dbstate.RoleAgent, MCPGrantKind: dbstate.GrantCoordination,
	}); err != nil {
		r.t.Fatal(err)
	}
	if kagentSessionID != "" {
		r.exec(`UPDATE agent_bindings SET kagent_session_id = $2, kagent_agent_namespace = 'kagent', kagent_agent_name = 'agent' WHERE session_id = $1`,
			bindingID, kagentSessionID)
	}
	r.attempt(taskID, dbstate.AttemptSupervisor, dbstate.AttemptActive, bindingID)
	r.setStatus(taskID, "running", "")
	return taskID, bindingID
}

func TestLivenessBlocksATaskWhoseSessionIsGone(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{}) // Exists: false, kagent has no record of it
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "blocked" {
		t.Fatalf("status = %s, want blocked", got)
	}
	if got := r.taskReason(id); got != "session_lost" {
		t.Fatalf("reason = %s, want session_lost", got)
	}
}

func TestLivenessBlocksATaskWhoseSessionIsTerminal(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{Exists: true, Terminal: true})
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "blocked" {
		t.Fatalf("status = %s, want blocked", got)
	}
	if got := r.taskReason(id); got != "session_lost" {
		t.Fatalf("reason = %s, want session_lost", got)
	}
}

// Review P0: the sweep must resolve the binding's kagent_session_id and ask kagent about that, not
// about Mainloop's own binding id, which kagent never issued and would answer NotFound for.
func TestLivenessAsksKagentForTheResolvedSessionIdNotTheBindingId(t *testing.T) {
	r := newRig(t)
	id, bindingID := r.liveTask("ks-1")
	r.live.set(bindingID, kagent.SessionLiveness{}) // wrong key: answers gone if asked by binding id
	r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "running" {
		t.Fatalf("status = %s, want running (the sweep must have asked for ks-1, not %s)", got, bindingID)
	}
}

// Review P0: a coordination attempt has no kagent session until its brief is delivered. That must
// not read as lost.
func TestLivenessSkipsAnAttemptWithNoKagentSessionYet(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveTask("") // no kagent_session_id yet
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "running" {
		t.Fatalf("status = %s, want running", got)
	}
	if n := r.live.lookCount(); n != 0 {
		t.Fatalf("asked kagent about a session that does not exist yet: calls=%d", n)
	}
}

func TestLivenessBlocksAStalledTask(t *testing.T) {
	r := newRig(t)
	id, bindingID := r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
	r.activity.set(bindingID, time.Now().Add(-46*time.Minute))
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "blocked" {
		t.Fatalf("status = %s, want blocked", got)
	}
	if got := r.taskReason(id); got != "stalled" {
		t.Fatalf("reason = %s, want stalled", got)
	}
}

// kagent keeps a turn "working" until it ends, and a turn whose native process wedged never ends,
// so a working task is not proof of life. Prod incident: a Claude turn hung inside a tool call at
// 14:58, kagent reported the task working with a 14:48 status time for five hours, and the sweep
// read that as activity every minute. What counts is kagent recording something new for the turn.
//
// The sweep has no clock of its own beyond Service.Now, so these tests move it: the first look at a
// working turn starts its stall clock, and later looks find it changed or not.
func stuckTurn(old time.Time, newest string) []*a2a.Task {
	return []*a2a.Task{{
		ID:      "t-1",
		Status:  a2a.TaskStatus{State: a2a.TaskStateWorking, Timestamp: &old},
		History: []*a2a.Message{{ID: newest}},
	}}
}

// harnessTurn is a working turn the way the kagent harness records it: one brief in the history and
// the status time of the turn's start, both fixed for the whole turn, while the text and tool calls
// it produces arrive as artifacts. n artifacts of one part each are listed, and extra further parts
// were appended to the last one (streamed text).
func harnessTurn(start time.Time, n, extra int) []*a2a.Task {
	task := &a2a.Task{
		ID:      "t-1",
		Status:  a2a.TaskStatus{State: a2a.TaskStateWorking, Timestamp: &start},
		History: []*a2a.Message{{ID: "brief"}},
	}
	for i := range n {
		task.Artifacts = append(task.Artifacts, &a2a.Artifact{
			ID: a2a.ArtifactID("a-" + strconv.Itoa(i)), Parts: a2a.ContentParts{a2a.NewTextPart("x")},
		})
	}
	if n > 0 {
		last := task.Artifacts[n-1]
		for range extra {
			last.Parts = append(last.Parts, a2a.NewTextPart("x"))
		}
	}
	return []*a2a.Task{task}
}

func TestLivenessBlocksAWorkingTurnThatRecordsNothingNew(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
	start := time.Now()
	old := start.Add(-6 * time.Hour)
	r.live.setTasks("ks-1", stuckTurn(old, "m-1"))

	r.setClock(start)
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "running" {
		t.Fatalf("status = %s, want running (the first look only starts the clock)", got)
	}

	r.setClock(start.Add(40 * time.Minute))
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "running" {
		t.Fatalf("status = %s, want running (40 minutes is inside the stall timeout)", got)
	}

	r.setClock(start.Add(46 * time.Minute))
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "blocked" {
		t.Fatalf("status = %s, want blocked (a turn kagent calls working but that recorded nothing for 46 minutes)", got)
	}
	if got := r.taskReason(id); got != "stalled" {
		t.Fatalf("reason = %s, want stalled", got)
	}
}

func TestLivenessKeepsAWorkingTurnThatKeepsRecordingProgress(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
	start := time.Now()
	old := start.Add(-6 * time.Hour)

	// Every half hour a new message lands: three hours of a busy turn.
	for i := range 7 {
		r.live.setTasks("ks-1", stuckTurn(old, "m-"+string(rune('a'+i))))
		r.setClock(start.Add(time.Duration(i) * 30 * time.Minute))
		r.sweep(r.svc.LivenessSweep())
		if got := r.taskStatus(id); got != "running" {
			t.Fatalf("step %d: status = %s, want running (the turn is still recording progress)", i, got)
		}
	}
}

// Review P1: a healthy harness turn that runs for hours changes nothing but its artifacts, so the
// fingerprint must follow them. One new tool artifact every ten minutes for three hours is a busy
// turn, never a stalled one.
func TestReviewHealthyHarnessTurnIsNotStalled(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
	start := time.Now()
	turnStart := start.Add(-time.Minute)

	for i := range 19 {
		r.live.setTasks("ks-1", harnessTurn(turnStart, i, 0))
		r.setClock(start.Add(time.Duration(i) * 10 * time.Minute))
		r.sweep(r.svc.LivenessSweep())
		if got := r.taskStatus(id); got != "running" {
			t.Fatalf("minute %d: status = %s, want running (the turn keeps recording tool calls)", i*10, got)
		}
	}
}

// Streamed text appends parts to the artifact the turn already has, adding no new artifact.
func TestLivenessKeepsAHarnessTurnThatOnlyStreamsMoreText(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
	start := time.Now()

	for i := range 13 {
		r.live.setTasks("ks-1", harnessTurn(start.Add(-time.Minute), 1, i))
		r.setClock(start.Add(time.Duration(i) * 20 * time.Minute))
		r.sweep(r.svc.LivenessSweep())
		if got := r.taskStatus(id); got != "running" {
			t.Fatalf("minute %d: status = %s, want running (the text is still streaming)", i*20, got)
		}
	}
}

// A turn that records no artifact either is as stuck as one that records nothing at all.
func TestLivenessBlocksAHarnessTurnWhoseArtifactsStopGrowing(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
	start := time.Now()
	r.live.setTasks("ks-1", harnessTurn(start.Add(-time.Hour), 4, 2))

	for _, at := range []time.Duration{0, 30 * time.Minute} {
		r.setClock(start.Add(at))
		r.sweep(r.svc.LivenessSweep())
		if got := r.taskStatus(id); got != "running" {
			t.Fatalf("+%s: status = %s, want running", at, got)
		}
	}
	r.setClock(start.Add(46 * time.Minute))
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskReason(id); got != "stalled" {
		t.Fatalf("reason = %q, want stalled (nothing new for 46 minutes)", got)
	}
}

// Review P1: a progress report is proof of life. When the sweep was wrong and a report recovers the
// task, the next sweep must not block it again over the same old fingerprint.
func TestReviewReportAfterFalseStallIsNotBlockedAgainNextSweep(t *testing.T) {
	r := newRig(t)
	id, binding := r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
	start := time.Now()
	r.live.setTasks("ks-1", harnessTurn(start, 2, 0)) // does not change from here on

	r.setClock(start)
	r.sweep(r.svc.LivenessSweep())
	r.setClock(start.Add(46 * time.Minute))
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskReason(id); got != "stalled" {
		t.Fatalf("reason = %q, want stalled", got)
	}

	agent := r.agent(dbstate.RoleSupervisor, binding)
	reportAt := start.Add(47 * time.Minute)
	r.setClock(reportAt)
	if _, err := r.svc.Report(context.Background(), agent, report(agent, "r1", dbstate.OutcomeProgress)); err != nil {
		t.Fatal(err)
	}
	if got := r.taskStatus(id); got != "running" {
		t.Fatalf("status after the report = %s, want running", got)
	}

	r.setClock(reportAt.Add(time.Minute))
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "running" {
		t.Fatalf("status = %s a minute after the report, want running (the report restarted the clock)", got)
	}

	r.setClock(reportAt.Add(46 * time.Minute))
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskReason(id); got != "stalled" {
		t.Fatalf("reason = %q, want stalled again after another silent window", got)
	}
}

func TestLivenessRecordsTheProgressItSaw(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
	start := time.Now().Truncate(time.Microsecond)
	r.live.setTasks("ks-1", stuckTurn(start.Add(-time.Hour), "m-1"))
	r.setClock(start)
	r.sweep(r.svc.LivenessSweep())

	var marker *string
	var at *time.Time
	if err := r.pool.QueryRow(context.Background(),
		`SELECT progress_marker, progress_at FROM task_attempts WHERE task_id = $1`, id).Scan(&marker, &at); err != nil {
		t.Fatal(err)
	}
	if marker == nil || *marker == "" || at == nil || !at.Equal(start) {
		t.Fatalf("progress = %v at %v, want a marker recorded at %v", marker, at, start)
	}

	// Seeing the same thing again must not move the time it was first seen.
	r.setClock(start.Add(10 * time.Minute))
	r.sweep(r.svc.LivenessSweep())
	if err := r.pool.QueryRow(context.Background(),
		`SELECT progress_marker, progress_at FROM task_attempts WHERE task_id = $1`, id).Scan(&marker, &at); err != nil {
		t.Fatal(err)
	}
	if at == nil || !at.Equal(start) {
		t.Fatalf("progress_at = %v, want it left at %v", at, start)
	}
}

// P2: an agent waiting on the owner (input-required or auth-required) is not stalled either — it
// is blocked on a reply, not idle — though the runtime bound still applies regardless of activity.
func TestLivenessWaitingOnTheOwnerResetsTheStallClock(t *testing.T) {
	for _, state := range []a2a.TaskState{a2a.TaskStateInputRequired, a2a.TaskStateAuthRequired} {
		t.Run(string(state), func(t *testing.T) {
			r := newRig(t)
			id, bindingID := r.liveTask("ks-1")
			r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
			r.activity.set(bindingID, time.Now().Add(-46*time.Minute)) // would stall on its own
			old := time.Now().Add(-50 * time.Minute)
			r.live.setTasks("ks-1", []*a2a.Task{{ID: "t-1", Status: a2a.TaskStatus{State: state, Timestamp: &old}}})
			r.sweep(r.svc.LivenessSweep())
			if got := r.taskStatus(id); got != "running" {
				t.Fatalf("status = %s, want running (waiting on the owner is activity now)", got)
			}
		})
	}
}

func TestLivenessBlocksATaskPastItsRuntimeBound(t *testing.T) {
	r := newRig(t)
	id, bindingID := r.liveTask("ks-1")
	r.exec(`UPDATE task_attempts SET created_at = now() - interval '7 hours' WHERE task_id = $1`, id)
	// The session itself looks healthy and recently active: only the overall bound catches this one.
	r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
	r.activity.set(bindingID, time.Now())
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "blocked" {
		t.Fatalf("status = %s, want blocked", got)
	}
	if got := r.taskReason(id); got != "runtime_limit" {
		t.Fatalf("reason = %s, want runtime_limit", got)
	}
}

// Review P2: the observation is a slow, uncached network call made with no lock held. If a report
// reaches the task first — completing it, or blocking it on an owner question — that report owns
// the task, and the liveness signal that raced it must be dropped, not overwrite it.
func TestLivenessRaceWithACompletingReportLeavesTheTaskAlone(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{}) // would otherwise block it session_lost
	r.live.onObserve = func() {
		r.exec(`UPDATE tasks SET status = 'waiting', reason = 'publication', version = version + 1 WHERE id = $1`, id)
	}
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "waiting" {
		t.Fatalf("status = %s, want waiting (the report that landed during the observation owns the task)", got)
	}
	if got := r.taskReason(id); got != "publication" {
		t.Fatalf("reason = %s, want publication", got)
	}
}

// Once a task is blocked, the sweep's own query stops selecting it: looking again costs nothing and
// changes nothing.
func TestLivenessSweepLeavesABlockedTaskAlone(t *testing.T) {
	r := newRig(t)
	r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{})
	r.sweep(r.svc.LivenessSweep())
	n := r.live.lookCount()
	r.sweep(r.svc.LivenessSweep())
	if r.live.lookCount() != n {
		t.Fatal("the sweep looked at an already-blocked task's session again")
	}
}

// setClock moves the sweep's clock and the clock KagentLiveness stamps "now" with together, as one
// process clock does in production.
func (r *rig) setClock(at time.Time) {
	now := func() time.Time { return at }
	r.svc.Now = now
	r.svc.Liveness.(*tasks.KagentLiveness).Now = now
}

// unreadableList answers ObserveLiveness like the rig's fake but fails the task reads the way the
// kagent client does when a reply is over its size limit: the plain list, or with inState the list of
// the unfinished tasks with their artifacts.
type unreadableList struct {
	*fakeKagentLiveness
	inState bool
}

var errUnreadable = &kagent.OutcomeUnknown{Op: "A2A ListTasks", Detail: "reply unreadable"}

func (u unreadableList) ListTasksLatest(ctx context.Context, agent kagent.AgentRef, contextID string) ([]*a2a.Task, error) {
	if u.inState {
		return u.fakeKagentLiveness.ListTasksLatest(ctx, agent, contextID)
	}
	return nil, errUnreadable
}

func (u unreadableList) ListTasksInStateLatest(ctx context.Context, agent kagent.AgentRef, contextID string, state a2a.TaskState) ([]*a2a.Task, error) {
	if u.inState {
		return nil, errUnreadable
	}
	return u.fakeKagentLiveness.ListTasksInStateLatest(ctx, agent, contextID, state)
}

func (r *rig) readsFailing(inState bool) {
	r.svc.Liveness = &tasks.KagentLiveness{
		Pool: r.pool, OwnerID: owner, Sessions: r.ses.Service, Kagent: unreadableList{r.live, inState}, Activity: r.activity,
	}
}

// Review P2: a task read that fails used to leave the attempt untouched by every bound, the runtime
// limit included, so a conversation too big to read was never bounded at all.
func TestLivenessAFailedTaskReadStillAppliesTheRuntimeBound(t *testing.T) {
	for name, inState := range map[string]bool{"plain list": false, "list with artifacts": true} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			id, _ := r.liveTask("ks-1")
			r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
			r.live.setTasks("ks-1", harnessTurn(time.Now(), 1, 0))
			r.readsFailing(inState)
			r.setClock(time.Now().Add(7 * time.Hour))
			r.sweep(r.svc.LivenessSweep())
			if got := r.taskReason(id); got != "runtime_limit" {
				t.Fatalf("7 hours in, task read failing: status = %s reason = %q, want blocked runtime_limit", r.taskStatus(id), got)
			}
		})
	}
}

// The same bound holds for an attempt that has no kagent session to read.
func TestLivenessAnAttemptWithNoKagentSessionIsStillBoundedByRuntime(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveTask("")
	r.setClock(time.Now().Add(7 * time.Hour))
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskReason(id); got != "runtime_limit" {
		t.Fatalf("status = %s reason = %q, want blocked runtime_limit", r.taskStatus(id), got)
	}
}

// Review P2: one failed read says nothing, but reads that keep failing for the stall timeout leave the
// attempt as unjudged as a silent one, and it is blocked as stalled; a read that works in between
// starts the count over.
func TestLivenessReadsThatKeepFailingBlockTheTaskAsStalled(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
	r.live.setTasks("ks-1", harnessTurn(time.Now(), 1, 0))
	healthy := r.svc.Liveness
	start := time.Now()
	look := func(after time.Duration) {
		r.setClock(start.Add(after))
		r.sweep(r.svc.LivenessSweep())
	}

	r.readsFailing(false)
	look(0)
	look(30 * time.Minute)
	if got := r.taskStatus(id); got != "running" {
		t.Fatalf("30 minutes of failed reads: status = %s, want running", got)
	}

	// A read that works clears the count; reads failing again start a new one.
	r.svc.Liveness = healthy
	look(31 * time.Minute)
	r.readsFailing(false)
	look(50 * time.Minute)
	look(90 * time.Minute)
	if got := r.taskStatus(id); got != "running" {
		t.Fatalf("40 minutes into a new run of failed reads: status = %s, want running", got)
	}

	look(96 * time.Minute)
	if got := r.taskReason(id); got != "stalled" {
		t.Fatalf("46 minutes of failed reads: status = %s reason = %q, want blocked stalled", r.taskStatus(id), got)
	}
}

// Review P2: artifacts can run to megabytes in a long conversation, and only a task that can still
// change has any use for them. Finished tasks are listed without; a conversation with no unfinished
// task reads no artifacts at all.
func TestLivenessReadsArtifactsOnlyForUnfinishedTasks(t *testing.T) {
	r := newRig(t)
	r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
	done := time.Now().Add(-time.Hour)
	finished := &a2a.Task{ID: "t-0", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted, Timestamp: &done}}
	r.live.setTasks("ks-1", append([]*a2a.Task{finished}, harnessTurn(time.Now(), 1, 0)...))
	r.sweep(r.svc.LivenessSweep())
	if got := r.live.artifactStatesRead(); len(got) != 1 || got[0] != a2a.TaskStateWorking {
		t.Fatalf("artifacts read for states %v, want only working", got)
	}

	r.live.setTasks("ks-1", []*a2a.Task{finished})
	r.live.resetArtifactReads()
	r.sweep(r.svc.LivenessSweep())
	if got := r.live.artifactStatesRead(); len(got) != 0 {
		t.Fatalf("artifacts read for states %v with every task finished, want none", got)
	}
}

// Review P2: a report recovers a task blocked for failed reads, and as long as the reads keep failing
// the next sweep must not block it again over the failure that began before the report. The window
// counts from the report, as the stall clock does.
func TestReportAfterReadFailureStallIsNotBlockedAgainNextSweep(t *testing.T) {
	r := newRig(t)
	id, binding := r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
	r.readsFailing(false)
	start := time.Now()
	look := func(after time.Duration) {
		r.setClock(start.Add(after))
		r.sweep(r.svc.LivenessSweep())
	}

	look(0)
	look(46 * time.Minute)
	if got := r.taskReason(id); got != "stalled" {
		t.Fatalf("reason = %q after 46 minutes of failed reads, want stalled", got)
	}

	agent := r.agent(dbstate.RoleSupervisor, binding)
	r.setClock(start.Add(47 * time.Minute))
	if _, err := r.svc.Report(context.Background(), agent, report(agent, "r1", dbstate.OutcomeProgress)); err != nil {
		t.Fatal(err)
	}
	if got := r.taskStatus(id); got != "running" {
		t.Fatalf("status after the report = %s, want running", got)
	}

	look(48 * time.Minute)
	look(90 * time.Minute)
	if got := r.taskStatus(id); got != "running" {
		t.Fatalf("status = %s reason = %q 43 minutes after the report with reads still failing, want running", got, r.taskReason(id))
	}

	look(94 * time.Minute)
	if got := r.taskReason(id); got != "stalled" {
		t.Fatalf("reason = %q after another silent window of failed reads, want stalled again", got)
	}
}
