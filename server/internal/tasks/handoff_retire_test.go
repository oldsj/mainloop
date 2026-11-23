package tasks_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/tasks"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1"
)

// forcingStops confirms every stop and records each Retire: the session and how many attempts the
// session's task had then (a successor would be the second).
type forcingStops struct {
	r     *rig
	mu    sync.Mutex
	calls []string
}

func (f *forcingStops) Stopped(context.Context, string) (bool, error) { return true, nil }

func (f *forcingStops) Retire(_ context.Context, session string) error {
	n := f.r.count(`SELECT count(*) FROM task_attempts WHERE task_id = (SELECT task_id FROM task_attempts WHERE session_id = $1)`, session)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, session+" attempts="+strconv.Itoa(n))
	return nil
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
	want := oldSession + " attempts=1"
	if got := f.seen(); len(got) != 1 || got[0] != want {
		t.Fatalf("retires = %v, want [%s]", got, want)
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
		t.Fatalf("retires = %v", got)
	}
}

// copyingNative is a kagent Session that settled after its cancel, deleted through the transcript
// copy: RetireSession fails without deleting when copyErr is set.
type copyingNative struct {
	fakeNativeSession
	rmu      sync.Mutex
	copyErr  error
	retired  []string
	forceDel int
}

func (f *copyingNative) RetireSession(_ context.Context, id string) (*kagentapi.Session, error) {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	if f.copyErr != nil {
		return nil, f.copyErr
	}
	f.retired = append(f.retired, id)
	return &kagentapi.Session{Id: id}, nil
}

func (f *copyingNative) retires() []string {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	return append([]string(nil), f.retired...)
}

func retireRig(t *testing.T, copyErr error) (r *rig, n *copyingNative, id, oldSession string) {
	t.Helper()
	r = newRig(t)
	n = &copyingNative{copyErr: copyErr}
	n.live = kagent.SessionLiveness{Exists: true, Terminal: true} // its turn settled cancelled
	r.svc.StopObserver = &tasks.KagentStops{Pool: r.pool, OwnerID: owner, Sessions: r.ses.Service, Liveness: n, Deleter: n}
	r.run()
	id, oldSession, _ = r.runningStanding()
	r.bindKagent(oldSession, "ks-old")
	return r, n, id, oldSession
}

func TestARetiredSessionIsDeletedOnlyThroughTheTranscriptCopy(t *testing.T) {
	r, n, id, oldSession := retireRig(t, nil)

	// When the supervisor is restarted
	r.restart("rs-1", false)
	r.handedOff(id)

	// Then its kagent Session went through the copy-then-delete, never the plain force delete
	if got := n.retires(); len(got) != 1 || got[0] != "ks-old" {
		t.Fatalf("retired = %v", got)
	}
	if d := n.deleteCount(); d != 0 {
		t.Fatalf("force-deleted %d times without the copy", d)
	}
	// And the delete is recorded on the old attempt and its binding
	if c := r.count(`SELECT count(*) FROM task_attempts WHERE session_id = $1 AND native_deleted_at IS NOT NULL`, oldSession); c != 1 {
		t.Fatal("the old attempt does not record the delete")
	}
	if c := r.count(`SELECT count(*) FROM agent_bindings WHERE session_id = $1 AND kagent_deleted_at IS NOT NULL`, oldSession); c != 1 {
		t.Fatal("the old binding does not record the delete")
	}
}

func TestASessionWhoseCopyFailsIsNotRetired(t *testing.T) {
	r, n, id, oldSession := retireRig(t, errors.New("transcripts: copy before retire: kagent hiccup"))

	// When the supervisor is restarted and its transcript cannot be copied
	r.restart("rs-1", false)

	// Then the handoff still completes, and the old Session is left to kagent, undeleted
	r.handedOff(id)
	if len(n.retires()) != 0 || n.deleteCount() != 0 {
		t.Fatalf("retired %v, force-deleted %d", n.retires(), n.deleteCount())
	}
	if c := r.count(`SELECT count(*) FROM task_attempts WHERE session_id = $1 AND native_deleted_at IS NOT NULL`, oldSession); c != 0 {
		t.Fatal("an undeleted session is recorded deleted")
	}
}
