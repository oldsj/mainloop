package sessions_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/dotfiles"
	"github.com/oldsj/mainloop/server/internal/dotfiles/dotfilestest"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1"
)

// wantDotfiles is what a create sends for dotfilestest's ready pin, downloaded from origin.
func wantDotfiles(origin string) *kagentapi.Dotfiles {
	return &kagentapi.Dotfiles{
		Url: origin + dotfiles.BundlePath + dotfilestest.Digest(), Digest: dotfilestest.Digest(),
		Repository: "example/dotfiles", Commit: dotfilestest.Commit, Path: "home/.agents",
	}
}

func TestTheMainThreadPinsItsDotfilesBeforeItsCreate(t *testing.T) {
	e := newEnv(t)
	pins := &dotfilestest.Pinner{Pool: e.pool, Creates: func() int { return len(e.srv.CreateRequests()) }}
	e.svc.Dotfiles = pins
	e.run()
	e.waitState(e.send("hello"), dbstate.DeliveryCompleted)

	m, err := e.svc.EnsureMain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reqs := e.srv.CreateRequests()
	calls := pins.Calls()
	if len(reqs) != 1 || len(calls) == 0 {
		t.Fatalf("creates = %d, pins = %v", len(reqs), calls)
	}
	for _, c := range calls {
		if c.Session != m.SessionID || c.Request != reqs[0].GetRequestId() {
			t.Errorf("pinned %+v, want session %s and request %s", c, m.SessionID, reqs[0].GetRequestId())
		}
	}
	if calls[0].CreatesBefore != 0 {
		t.Errorf("the first pin ran after %d creates, want before any", calls[0].CreatesBefore)
	}
	// Without an MCP credential to authorize the download, the create sends no dotfiles.
	if calls[0].MCPOrigin != "" || reqs[0].GetDotfiles() != nil {
		t.Errorf("a create without a credential pinned with origin %q and sent %v", calls[0].MCPOrigin, reqs[0].GetDotfiles())
	}
}

func TestAFailedPinSendsNoCreate(t *testing.T) {
	e := newEnv(t)
	pins := &dotfilestest.Pinner{Creates: func() int { return len(e.srv.CreateRequests()) }, Err: errors.New("database down")}
	e.svc.Dotfiles = pins
	e.run()
	e.send("hello")

	deadline := time.Now().Add(10 * time.Second)
	for len(pins.Calls()) == 0 {
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

// The main thread's create carries its MCP credential, so it sends the pinned bundle at its url on
// the credential's origin, and the pin records that url.
func TestTheMainThreadIsCreatedWithItsDotfiles(t *testing.T) {
	e := newEnv(t)
	e.withCredentials()
	pins := &dotfilestest.Pinner{Pool: e.pool, Ready: true}
	e.svc.Dotfiles = pins
	e.run()
	e.waitState(e.send("hello"), dbstate.DeliveryCompleted)

	reqs := e.srv.CreateRequests()
	if len(reqs) != 1 || len(reqs[0].GetCredentials()) != 1 {
		t.Fatalf("create requests = %v", reqs)
	}
	if got := reqs[0].GetDotfiles(); !proto.Equal(got, wantDotfiles(mcpOrigin)) {
		t.Fatalf("dotfiles = %v, want %v", got, wantDotfiles(mcpOrigin))
	}
	if calls := pins.Calls(); len(calls) == 0 || calls[0].MCPOrigin != mcpOrigin {
		t.Fatalf("pins = %+v", calls)
	}
	sent, err := dotfiles.Sent(context.Background(), dbq.New(e.pool), reqs[0].GetRequestId())
	if err != nil || !proto.Equal(sent, reqs[0].GetDotfiles()) {
		t.Fatalf("recorded = %v, %v", sent, err)
	}
}

// A create whose reply was lost is replayed with the dotfiles it was sent with: kagent refuses a
// replay that differs, and the fake does too.
func TestACreateReplaySendsTheSameDotfiles(t *testing.T) {
	e, k := newFlakyEnv(t)
	e.withCredentials()
	e.svc.Dotfiles = &dotfilestest.Pinner{Pool: e.pool, Ready: true}
	// A session that reaches MCP, so its create carries a credential to fetch the bundle with.
	_, req := e.failedCreateThenCancelAndArchive(k, func(id string) {
		if _, err := dbq.New(e.pool).SetBindingGrant(context.Background(), dbq.SetBindingGrantParams{SessionID: id, MCPGrantKind: dbstate.GrantWorkspace}); err != nil {
			t.Fatal(err)
		}
	})
	k.down.Store(false)
	first := k.last.Load()
	if !proto.Equal(first.GetDotfiles(), wantDotfiles(mcpOrigin)) || first.GetRequestId() != req {
		t.Fatalf("the create sent %v for request %s", first.GetDotfiles(), first.GetRequestId())
	}
	// The create reached kagent and only the reply was lost.
	if _, err := k.Kagent.CreateSession(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ResolveCreates(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.unresolved() != 0 {
		t.Fatal("the replay did not settle the create")
	}
	reqs := e.srv.CreateRequests()
	if len(reqs) < 2 {
		t.Fatalf("%d creates reached kagent, want the original and the replay", len(reqs))
	}
	for _, r := range reqs {
		if r.GetRequestId() == req && !proto.Equal(r.GetDotfiles(), first.GetDotfiles()) {
			t.Fatalf("a replay sent %v, want %v", r.GetDotfiles(), first.GetDotfiles())
		}
	}
	if _, ok := e.srv.Session("session-1"); ok {
		t.Fatal("the replay found another Session than the one the create made")
	}
}

// A provider switch makes a new native Session for the main thread under a new request: it is
// pinned anew and sends its dotfiles with its own credential.
func TestAProviderSwitchSendsTheNewSessionsDotfiles(t *testing.T) {
	e := providerEnv(t)
	e.withCredentials()
	pins := &dotfilestest.Pinner{Pool: e.pool, Ready: true}
	e.svc.Dotfiles = pins
	stop, _ := e.warm()
	defer stop()
	oldRequest := e.srv.CreateRequests()[0].GetRequestId()

	if st := e.mustSelect("codex", false); st.Status != "ready" {
		t.Fatalf("state = %+v", st)
	}
	reqs := e.srv.CreateRequests()
	last := reqs[len(reqs)-1]
	if last.GetAgent().GetName() != codexAgentName || last.GetRequestId() == oldRequest {
		t.Fatalf("Codex create = %v", last)
	}
	for _, r := range reqs {
		if !proto.Equal(r.GetDotfiles(), wantDotfiles(mcpOrigin)) {
			t.Fatalf("create %s sent %v", r.GetRequestId(), r.GetDotfiles())
		}
	}
	requests := map[string]bool{}
	for _, c := range pins.Calls() {
		requests[c.Request] = true
	}
	if !requests[oldRequest] || !requests[last.GetRequestId()] {
		t.Fatalf("pinned requests = %v, want both Sessions'", requests)
	}
}
