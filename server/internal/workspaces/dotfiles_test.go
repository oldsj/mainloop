package workspaces

import (
	"context"
	"sync"
	"testing"

	"github.com/oldsj/mainloop/server/internal/dotfiles"
)

type pinCall struct {
	session, request string
	createsBefore    int
}

type fakePinner struct {
	mu      sync.Mutex
	calls   []pinCall
	creates func() int
}

func (f *fakePinner) Pin(_ context.Context, session, request string) (dotfiles.Pinned, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, pinCall{session: session, request: request, createsBefore: f.creates()})
	return dotfiles.Pinned{Status: dotfiles.StatusNone}, nil
}

func TestAWorkspacePinsItsDotfilesBeforeItsCreate(t *testing.T) {
	h := newHarness(t, nil)
	pins := &fakePinner{creates: func() int { return len(h.fake.CreateRequests()) }}
	h.svc.Dotfiles = pins
	w := h.running()

	reqs := h.fake.CreateRequests()
	pins.mu.Lock()
	calls := append([]pinCall(nil), pins.calls...)
	pins.mu.Unlock()
	if len(reqs) != 1 || len(calls) == 0 {
		t.Fatalf("creates = %d, pins = %v", len(reqs), calls)
	}
	for _, c := range calls {
		if c.session != w.WorkspaceId || c.request != reqs[0].GetRequestId() {
			t.Errorf("pinned %+v, want workspace %s and request %s", c, w.WorkspaceId, reqs[0].GetRequestId())
		}
	}
	if calls[0].createsBefore != 0 {
		t.Errorf("the first pin ran after %d creates, want before any", calls[0].createsBefore)
	}
}
