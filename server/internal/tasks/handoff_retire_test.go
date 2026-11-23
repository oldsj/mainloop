package tasks_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/oldsj/mainloop/server/internal/tasks"
)

// forcingStops confirms every stop and records each ForceStop: the session, the level, and how many
// attempts the session's task had then (a successor would be the second).
type forcingStops struct {
	r     *rig
	mu    sync.Mutex
	calls []string
}

func (f *forcingStops) Stopped(context.Context, string) (bool, error) { return true, nil }

func (f *forcingStops) ForceStop(_ context.Context, session string, level tasks.ForceLevel) (bool, error) {
	n := f.r.count(`SELECT count(*) FROM task_attempts WHERE task_id = (SELECT task_id FROM task_attempts WHERE session_id = $1)`, session)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, session+" "+level.String()+" attempts="+string(rune('0'+n)))
	return true, nil
}

func (f *forcingStops) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func TestARestartRetiresTheOldSessionBeforeItsSuccessorExists(t *testing.T) {
	r := newRig(t)
	f := &forcingStops{r: r}
	r.svc.StopObserver = f
	r.run()
	id, oldSession, _ := r.runningStanding()

	// When the supervisor is restarted
	r.restart("rs-1", false)
	r.handedOff(id)

	// Then its old native session was deleted once its stop was confirmed, before the successor was
	// made, so the handoff never needs a second actor's room
	want := oldSession + " " + tasks.ForceDelete.String() + " attempts=1"
	if got := f.seen(); len(got) != 1 || got[0] != want {
		t.Fatalf("force stops = %v, want [%s]", got, want)
	}
}

func TestACodeRetryKeepsTheOldActor(t *testing.T) {
	r := newRig(t)
	f := &forcingStops{r: r}
	r.svc.StopObserver = f
	head := strings.Repeat("a", 40)
	id, _, _ := r.codeHandoffTask(&fakeBranches{tips: map[string]string{"feature": head}})

	// When a code task is retried
	if _, err := r.svc.Retry(context.Background(), r.own, action(id, r.view(r.own, id), "retry-1")); err != nil {
		t.Fatal(err)
	}
	r.handedOff(id)

	// Then its old actor (and the workspace it holds) is left to kagent
	if got := f.seen(); len(got) != 0 {
		t.Fatalf("force stops = %v", got)
	}
}
