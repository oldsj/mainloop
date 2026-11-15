package workspaces

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/dotfiles"
	"github.com/oldsj/mainloop/server/internal/dotfiles/dotfilestest"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1"
)

func TestAWorkspacePinsItsDotfilesBeforeItsCreate(t *testing.T) {
	h := newHarness(t, nil)
	pins := &dotfilestest.Pinner{Pool: h.svc.Pool, Creates: func() int { return len(h.fake.CreateRequests()) }}
	h.svc.Dotfiles = pins
	w := h.running()

	reqs := h.fake.CreateRequests()
	calls := pins.Calls()
	if len(reqs) != 1 || len(calls) == 0 {
		t.Fatalf("creates = %d, pins = %v", len(reqs), calls)
	}
	for _, c := range calls {
		if c.Session != w.WorkspaceId || c.Request != reqs[0].GetRequestId() {
			t.Errorf("pinned %+v, want workspace %s and request %s", c, w.WorkspaceId, reqs[0].GetRequestId())
		}
	}
	if calls[0].CreatesBefore != 0 {
		t.Errorf("the first pin ran after %d creates, want before any", calls[0].CreatesBefore)
	}
	if calls[0].MCPOrigin != "" || reqs[0].GetDotfiles() != nil {
		t.Errorf("a create without a credential pinned with origin %q and sent %v", calls[0].MCPOrigin, reqs[0].GetDotfiles())
	}
}

// A workspace create carries its MCP credential and sends the pinned bundle on its origin; a delete
// that replays the create to find a Session it never saw sends the same dotfiles, read back from the
// pin (the workspace's purge then removes it).
func TestAWorkspaceCreateAndItsReplaySendTheSameDotfiles(t *testing.T) {
	h := newHarness(t, slow)
	h.withCredentials()
	h.svc.Dotfiles = &dotfilestest.Pinner{Pool: h.svc.Pool, Ready: true}
	h.run()
	h.fake.FailNext(kagent.ProcCreateSession, kagenttest.Fault{Drop: true})
	w, err := h.create(api.CreateWorkspaceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	h.svc.Config.Workspaces.OperationWait = 5e9
	if err := h.svc.Delete(bg, w.WorkspaceId); err != nil {
		t.Fatal(err)
	}
	// The first create was dropped before kagent saw it; what reached kagent is the replay.
	reqs := h.fake.CreateRequests()
	if len(reqs) == 0 {
		t.Fatal("the delete did not replay the create")
	}
	origin := h.svc.Config.MCPOrigin()
	want := &kagentapi.Dotfiles{
		Url: origin + dotfiles.BundlePath + dotfilestest.Digest(), Digest: dotfilestest.Digest(),
		Repository: "example/dotfiles", Commit: dotfilestest.Commit, Path: "home/.agents",
	}
	for _, r := range reqs {
		if !proto.Equal(r.GetDotfiles(), want) {
			t.Fatalf("a create sent %v, want %v", r.GetDotfiles(), want)
		}
	}
}
