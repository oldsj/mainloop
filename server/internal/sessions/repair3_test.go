package sessions_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/sessions"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

func (e *env) waitFor(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *env) bindingKsid(id string) string {
	e.t.Helper()
	var s *string
	if err := e.pool.QueryRow(context.Background(), `SELECT kagent_session_id FROM agent_bindings WHERE session_id=$1`, id).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return deref(s)
}

// The main thread's first create carries its MCP credential and lands in kagent, but every reply is
// lost and the turn ends. The sweep's replay must carry the same credential: kagent refuses a
// replay whose credentials differ (AlreadyExists), which would leave the binding unresolved and the
// landed Session alive for ever.
func TestAMainCreateReplayCarriesTheCreatesCredential(t *testing.T) {
	e, k := newFlakyEnv(t)
	e.withCredentials()
	ctx := context.Background()
	k.down.Store(true)
	stop := e.run()
	e.send("hello")
	m, err := e.svc.EnsureMain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("create requested", func() bool { return e.createRequested(m.SessionID) && k.creates.Load() >= 1 })
	orig := k.last.Load()
	if len(orig.GetCredentials()) != 1 {
		t.Fatalf("the create carried %d credential references, want 1", len(orig.GetCredentials()))
	}
	landed, err := k.Kagent.CreateSession(ctx, orig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Stop(ctx, m.SessionID); err != nil {
		t.Fatal(err)
	}
	stop()
	k.down.Store(false)
	if err := e.svc.ResolveCreates(ctx); err != nil {
		t.Fatalf("ResolveCreates: %v", err)
	}
	replay := k.last.Load()
	if len(replay.GetCredentials()) != 1 || !proto.Equal(replay.GetCredentials()[0], orig.GetCredentials()[0]) {
		t.Fatalf("replay credentials = %v, create's = %v", replay.GetCredentials(), orig.GetCredentials())
	}
	if e.unresolved() != 0 {
		t.Fatalf("%d creates unresolved after the replay", e.unresolved())
	}
	if _, alive := e.srv.Session(landed.GetId()); alive {
		t.Error("the Session the lost-reply create made is still alive")
	}
	// The next main message gets a fresh Session.
	e.run()
	d := e.waitState(e.send("again"), dbstate.DeliveryCompleted)
	if e.bindingKsid(m.SessionID) == "" || e.bindingKsid(m.SessionID) == landed.GetId() {
		t.Errorf("after %s the binding names %q (old %q)", d.State, e.bindingKsid(m.SessionID), landed.GetId())
	}
}

// kagent answers a replay with AlreadyExists when its contract differs. That is not retryable, and
// the Session it names is left to the orphan sweep: the create counts as resolved.
func TestAnAlreadyExistsReplayCountsAsResolved(t *testing.T) {
	e, k := newFlakyEnv(t)
	sid, _ := e.failedCreateThenCancelAndArchive(k)
	k.down.Store(false)
	e.svc.Kagent = &answering{Kagent: k}
	if err := e.svc.ResolveCreates(context.Background()); err != nil {
		t.Fatalf("ResolveCreates: %v", err)
	}
	if e.unresolved() != 0 || e.createRequested(sid) {
		t.Fatal("an AlreadyExists replay left the create unresolved")
	}
}

type answering struct{ sessions.Kagent }

func (a *answering) CreateSession(context.Context, *kagentapi.CreateSessionRequest) (*kagentapi.Session, error) {
	return nil, &kagent.SessionError{Op: "CreateSession", Code: connect.CodeAlreadyExists, Detail: "request id was used for a different session"}
}

// gated holds the resolve sweep's replay (named "Mainloop session") after kagent answered it.
type gated struct {
	sessions.Kagent
	entered chan struct{}
	release chan struct{}
	once    atomic.Bool
}

func (g *gated) CreateSession(ctx context.Context, r *kagentapi.CreateSessionRequest) (*kagentapi.Session, error) {
	s, err := g.Kagent.CreateSession(ctx, r)
	if r.GetName() == "Mainloop session" && g.once.CompareAndSwap(false, true) {
		close(g.entered)
		<-g.release
	}
	return s, err
}

// The resolve sweep replays a finished create while the owner sends a new message to the same idle
// session. The sweep rotates the binding's request id first, so the new delivery creates its own
// Session and the sweep deletes only the one its replay returned.
func TestAMessageDuringTheResolveSweepKeepsItsSession(t *testing.T) {
	e, k := newFlakyEnv(t)
	ctx := context.Background()
	k.down.Store(true)
	stop := e.run()
	sid := e.newSession("first")
	e.waitFor("create requested", func() bool { return e.createRequested(sid) && k.creates.Load() >= 1 })
	stop()
	if _, err := e.pool.Exec(ctx, `UPDATE deliveries SET state='failed', detail='test', lease_owner=NULL, lease_until=NULL WHERE session_id=$1`, sid); err != nil {
		t.Fatal(err)
	}
	k.down.Store(false)
	landed, err := k.Kagent.CreateSession(ctx, k.last.Load())
	if err != nil {
		t.Fatal(err)
	}
	g := &gated{Kagent: k, entered: make(chan struct{}), release: make(chan struct{})}
	e.svc.Kagent = g
	done := make(chan error, 1)
	go func() { done <- e.svc.ResolveCreates(ctx) }()
	<-g.entered
	msg := uuid.NewString()
	if _, err := e.svc.SendTo(ctx, sid, msg, "second"); err != nil {
		t.Fatal(err)
	}
	e.run()
	e.waitState(msg, dbstate.DeliveryCompleted)
	bound := e.bindingKsid(sid)
	if bound == "" || bound == landed.GetId() {
		t.Fatalf("the second message bound %q (the lost-reply Session is %q)", bound, landed.GetId())
	}
	close(g.release)
	if err := <-done; err != nil {
		t.Logf("ResolveCreates: %v", err)
	}
	if e.bindingKsid(sid) != bound {
		t.Fatalf("the sweep changed the binding from %s to %s", bound, e.bindingKsid(sid))
	}
	if _, alive := e.srv.Session(bound); !alive {
		t.Fatal("the sweep deleted the Session the new message is using")
	}
	e.waitState(func() string {
		id := uuid.NewString()
		if _, err := e.svc.SendTo(ctx, sid, id, "third"); err != nil {
			t.Fatal(err)
		}
		return id
	}(), dbstate.DeliveryCompleted)
	if e.bindingKsid(sid) != bound {
		t.Errorf("the third message moved the binding to %s", e.bindingKsid(sid))
	}
}

// Waiting for an old credential's cleanup takes many claims. They are not create failures: one 503
// afterwards is retried, not final.
func TestWaitingForCredentialCleanupDoesNotUseTheCreateBudget(t *testing.T) {
	e := newEnv(t)
	a := e.withCredentials()
	k := &flaky{Kagent: e.svc.Kagent}
	e.svc.Kagent = k
	ctx := context.Background()
	m, err := e.svc.EnsureMain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	old, err := a.Creds.Issue(ctx, m.SessionID, dbstate.CredentialMCP)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Creds.Revoke(ctx, m.SessionID); err != nil {
		t.Fatal(err)
	}
	e.run()
	id := e.send("hello")
	e.waitFor("many claims", func() bool { return e.delivery(id).Attempts >= 8 })
	k.down.Store(true)
	if err := a.Cluster.CoreV1().Secrets(authtest.Namespace).Delete(ctx, old.SecretName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE credentials SET state='deleted' WHERE id=$1`, old.CredentialID); err != nil {
		t.Fatal(err)
	}
	e.waitFor("one failed create", func() bool { return k.creates.Load() >= 1 })
	k.down.Store(false)
	d := e.waitState(id, dbstate.DeliveryCompleted)
	if d.CreateFailures != 1 {
		t.Errorf("create_failures = %d, want 1 (claims: %d)", d.CreateFailures, d.Attempts)
	}
}

// The replay names the Agent stored on the binding when the create was requested, so it still
// finds (and deletes) the create's Session after the provider configuration has changed.
func TestAReplayNamesTheAgentTheCreateNamed(t *testing.T) {
	e, k := newFlakyEnv(t)
	sid, _ := e.failedCreateThenCancelAndArchive(k)
	k.down.Store(false)
	want := k.last.Load().GetAgent()
	var ns, name string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT kagent_create_agent_namespace, kagent_create_agent_name FROM agent_bindings WHERE session_id=$1`, sid).Scan(&ns, &name); err != nil {
		t.Fatal(err)
	}
	if ns != want.GetNamespace() || name != want.GetName() {
		t.Fatalf("stored Agent = %s/%s, the create named %s/%s", ns, name, want.GetNamespace(), want.GetName())
	}
	// The provider configuration is gone before the sweep runs.
	e.svc.Providers = nil
	if err := e.svc.ResolveCreates(context.Background()); err != nil {
		t.Fatalf("ResolveCreates: %v", err)
	}
	got := k.last.Load().GetAgent()
	if got.GetNamespace() != want.GetNamespace() || got.GetName() != want.GetName() {
		t.Fatalf("replay named %s/%s, want the create's %s/%s", got.GetNamespace(), got.GetName(), want.GetNamespace(), want.GetName())
	}
	if e.unresolved() != 0 || e.createRequested(sid) {
		t.Fatal("the create is still unresolved")
	}
}
