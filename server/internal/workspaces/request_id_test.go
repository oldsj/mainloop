package workspaces

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/config"
)

func TestARepeatedCreateWithTheSameRequestIDReturnsTheSameWorkspace(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.run()
	id := newID()
	body := api.CreateWorkspaceRequest{Repo: ptr("oldsj/mainloop")}

	first, err := h.svc.CreateWithRequestID(bg, id, body)
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.svc.CreateWithRequestID(bg, id, body)
	if err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if again.WorkspaceId != first.WorkspaceId || again.ObservedState != api.WorkspaceObservedStateRunning {
		t.Errorf("first %s, repeat %+v", first.WorkspaceId, again)
	}
	if n := h.workspaceCount(); n != 1 {
		t.Errorf("workspaces = %d", n)
	}
	reqs := h.fake.CreateRequests()
	if len(reqs) != 1 || reqs[0].GetRequestId() != id {
		t.Errorf("kagent create requests = %v, want one carrying the caller's id", reqs)
	}
	got, err := h.svc.LookupByRequestID(bg, id)
	if err != nil || got.WorkspaceId != first.WorkspaceId {
		t.Errorf("lookup = %+v, %v", got, err)
	}
}

func TestARepeatThatArrivesWhileTheCreateIsInFlightWaitsForIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	id := newID()
	body := api.CreateWorkspaceRequest{Repo: ptr("oldsj/mainloop")}
	// No runner yet: the first create returns with its operation still open.
	h.svc.Config.Workspaces.OperationWait = 0
	first, err := h.svc.CreateWithRequestID(bg, id, body)
	if err != nil {
		t.Fatal(err)
	}
	h.run()
	h.svc.Config.Workspaces.OperationWait = config.Seconds(time.Second)
	again, err := h.svc.CreateWithRequestID(bg, id, body)
	if err != nil {
		t.Fatal(err)
	}
	if again.WorkspaceId != first.WorkspaceId || again.ObservedState != api.WorkspaceObservedStateRunning {
		t.Errorf("repeat = %+v", again)
	}
	if n := len(h.fake.CreateRequests()); n != 1 {
		t.Errorf("kagent creates = %d", n)
	}
}

func TestConcurrentCreatesWithOneRequestIDMakeOneWorkspace(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.run()
	id := newID()
	const n = 8
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w, err := h.svc.CreateWithRequestID(bg, id, api.CreateWorkspaceRequest{Repo: ptr("oldsj/mainloop")})
			ids[i], errs[i] = w.WorkspaceId, err
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Errorf("call %d returned %s, call 0 returned %s", i, ids[i], ids[0])
		}
	}
	if c := h.workspaceCount(); c != 1 {
		t.Errorf("workspaces = %d", c)
	}
	if c := len(h.fake.CreateRequests()); c != 1 {
		t.Errorf("kagent creates = %d", c)
	}
}

func TestDifferentRequestIDsMakeDifferentWorkspaces(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.run()
	body := api.CreateWorkspaceRequest{Repo: ptr("oldsj/mainloop")}
	a, err := h.svc.CreateWithRequestID(bg, newID(), body)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.svc.CreateWithRequestID(bg, newID(), body)
	if err != nil {
		t.Fatal(err)
	}
	if a.WorkspaceId == b.WorkspaceId || h.workspaceCount() != 2 {
		t.Errorf("a %s b %s", a.WorkspaceId, b.WorkspaceId)
	}
}

func TestARequestIDMustBeAUUIDAndLookupMissesAreNotFound(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	var inv *InvalidError
	if _, err := h.svc.CreateWithRequestID(bg, "not-a-uuid", api.CreateWorkspaceRequest{Repo: ptr("oldsj/mainloop")}); !errors.As(err, &inv) {
		t.Errorf("err = %v, want *InvalidError", err)
	}
	for _, id := range []string{newID(), "not-a-uuid"} {
		if _, err := h.svc.LookupByRequestID(bg, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("lookup(%q) = %v", id, err)
		}
	}
	if h.workspaceCount() != 0 {
		t.Error("a rejected request id made a workspace")
	}
}
