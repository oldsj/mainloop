package sessions_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/dotfiles"
)

type pinCall struct {
	session, request string
	createsBefore    int
}

// fakePinner records each pin and how many creates kagent had seen when it ran.
type fakePinner struct {
	mu      sync.Mutex
	calls   []pinCall
	err     error
	creates func() int
}

func (f *fakePinner) Pin(_ context.Context, session, request string) (dotfiles.Pinned, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, pinCall{session: session, request: request, createsBefore: f.creates()})
	return dotfiles.Pinned{Status: dotfiles.StatusNone}, f.err
}

func (f *fakePinner) snapshot() []pinCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pinCall(nil), f.calls...)
}

func TestTheMainThreadPinsItsDotfilesBeforeItsCreate(t *testing.T) {
	e := newEnv(t)
	pins := &fakePinner{creates: func() int { return len(e.srv.CreateRequests()) }}
	e.svc.Dotfiles = pins
	e.run()
	e.waitState(e.send("hello"), dbstate.DeliveryCompleted)

	m, err := e.svc.EnsureMain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reqs := e.srv.CreateRequests()
	calls := pins.snapshot()
	if len(reqs) != 1 || len(calls) == 0 {
		t.Fatalf("creates = %d, pins = %v", len(reqs), calls)
	}
	for _, c := range calls {
		if c.session != m.SessionID || c.request != reqs[0].GetRequestId() {
			t.Errorf("pinned %+v, want session %s and request %s", c, m.SessionID, reqs[0].GetRequestId())
		}
	}
	if calls[0].createsBefore != 0 {
		t.Errorf("the first pin ran after %d creates, want before any", calls[0].createsBefore)
	}
}

func TestAFailedPinSendsNoCreate(t *testing.T) {
	e := newEnv(t)
	pins := &fakePinner{creates: func() int { return len(e.srv.CreateRequests()) }, err: errors.New("database down")}
	e.svc.Dotfiles = pins
	e.run()
	e.send("hello")

	deadline := time.Now().Add(10 * time.Second)
	for len(pins.snapshot()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the delivery never pinned")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(e.srv.CreateRequests()); n != 0 {
		t.Fatalf("%d creates were sent after the pin failed", n)
	}
}

func TestTheNativeViewShowsTheNewestPin(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m, err := e.svc.EnsureMain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	native := func() *api.SessionDotfiles {
		t.Helper()
		resp, err := e.API().GetSessionNative(ctx, api.GetSessionNativeRequestObject{SessionId: m.SessionID})
		if err != nil {
			t.Fatal(err)
		}
		return api.NativeSessionInfo(resp.(api.GetSessionNative200JSONResponse)).Dotfiles
	}
	if d := native(); d != nil {
		t.Fatalf("dotfiles before any pin = %+v", d)
	}

	q := dbq.New(e.pool)
	digest := "sha256:" + strings.Repeat("ab", 32)
	commit := strings.Repeat("c", 40)
	if err := q.InsertDotfilesBundle(ctx, dbq.InsertDotfilesBundleParams{Digest: digest, Content: []byte("tar")}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []dbq.InsertPinnedDotfilesParams{
		{RequestID: "old", SessionID: m.SessionID, Status: dotfiles.StatusNone},
		{
			RequestID: "new", SessionID: m.SessionID, Repository: "example/dotfiles", Ref: "main", Path: "home/.agents",
			Commit: &commit, Digest: &digest, Status: dotfiles.StatusReady,
		},
	} {
		if err := q.InsertPinnedDotfiles(ctx, p); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond) // distinct created_at
	}
	d := native()
	if d == nil || d.Status != api.SessionDotfilesStatusReady || d.Repository != "example/dotfiles" || d.Ref != "main" ||
		d.Path != "home/.agents" || d.Commit == nil || *d.Commit != commit || d.Digest == nil || *d.Digest != digest || d.Reason != "" {
		t.Fatalf("dotfiles = %+v", d)
	}
}

// A fork starts from its source's data, dotfiles included, so it records the source's pin.
func TestAForkRecordsItsSourcePin(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.completedSession()
	commit := strings.Repeat("d", 40)
	q := dbq.New(e.pool)
	if err := q.InsertPinnedDotfiles(ctx, dbq.InsertPinnedDotfilesParams{
		RequestID: "src", SessionID: id, Repository: "example/dotfiles", Commit: &commit,
		Status: dotfiles.StatusRefused, Reason: dotfiles.ReasonSecretFound,
	}); err != nil {
		t.Fatal(err)
	}
	cp := e.mustCheckpoint(id)
	out, err := e.forkCheckpoint(cp.Id, uuid.NewString(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := dotfiles.Latest(ctx, q, out.NewSessionId)
	want := dotfiles.Pinned{Repository: "example/dotfiles", Commit: commit, Status: dotfiles.StatusRefused, Reason: dotfiles.ReasonSecretFound}
	if err != nil || !ok || got != want {
		t.Fatalf("fork pin = %+v, %v, %v; want %+v", got, ok, err, want)
	}
}
