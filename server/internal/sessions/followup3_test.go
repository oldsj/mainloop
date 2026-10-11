package sessions_test

import (
	"context"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/workspaces"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// renameAgent points the service at a new Agent, as a configuration change would.
func (e *env) renameAgent(name string) {
	e.t.Helper()
	e.srv.AddAgent(e.svc.Agent.Namespace, name)
	e.svc.Agent = kagent.AgentRef{Namespace: e.svc.Agent.Namespace, Name: name}
}

// orphans is the workspace service's orphan sweep over this environment, run an hour ahead so no
// Session is too young to judge.
func (e *env) orphans(c *kagent.Client) workspaces.OrphanReport {
	e.t.Helper()
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "KAGENT_USER_ID": "mainloop"})
	if err != nil {
		e.t.Fatal(err)
	}
	ws := &workspaces.Service{
		Pool: e.pool, Kagent: c, Providers: e.svc.Providers, Config: cfg, Log: quiet(),
		Now: func() time.Time { return time.Now().Add(time.Hour) },
	}
	rep, err := ws.SweepOrphans(context.Background(), workspaces.OrphanOptions{Delete: true})
	if err != nil || rep.Blocked != "" {
		e.t.Fatalf("orphan sweep: blocked=%q err=%v", rep.Blocked, err)
	}
	return rep
}

// A main create lands and its reply is lost, then the main Agent's configuration changes before the
// next send. The send replays the create with the new Agent and kagent refuses it (AlreadyExists),
// so the turn replaces the request id and the message completes on a new Session on the renamed
// Agent. Nothing leaks: the orphan sweep reclaims the Session that landed on the old Agent. Messages
// sent before the next resolve pass complete too.
func TestAConfigChangeBetweenALostReplyAndTheNextSendCompletesOnTheRenamedAgent(t *testing.T) {
	e, k := newFlakyEnv(t)
	// The old Agent is not one of the provider Agents, so only the record of the create knows it.
	e.renameAgent("main-old")
	ctx := context.Background()
	k.down.Store(true)
	stop := e.run()
	e.send("hello")
	m, err := e.svc.EnsureMain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("create requested", func() bool { return e.createRequested(m.SessionID) && k.creates.Load() >= 1 })
	landed, err := k.Kagent.CreateSession(ctx, k.last.Load())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Stop(ctx, m.SessionID); err != nil {
		t.Fatal(err)
	}
	stop()
	k.down.Store(false)

	e.renameAgent("main-renamed")
	e.run()
	// Three messages before any resolve pass: none fails, all land on the renamed Agent.
	for _, text := range []string{"after the config change", "again, before the sweep", "and again"} {
		if d := e.waitState(e.send(text), dbstate.DeliveryCompleted); d.CreateFailures != 0 {
			t.Fatalf("%q: create failures = %d, want 0", text, d.CreateFailures)
		}
	}
	ksid := e.bindingKsid(m.SessionID)
	if ksid == "" || ksid == landed.GetId() {
		t.Fatalf("bound %q, want a new Session", ksid)
	}
	now, ok := e.srv.Session(ksid)
	if !ok || now.GetAgent().GetName() != "main-renamed" {
		t.Fatalf("new Session %v, want one on main-renamed", now.GetAgent())
	}
	if _, ok := e.srv.Session(landed.GetId()); !ok {
		t.Fatal("the Session on the old Agent was deleted by a send")
	}

	if err := e.svc.ResolveCreates(ctx); err != nil || e.unresolved() != 0 {
		t.Fatalf("ResolveCreates: %v, unresolved %d", err, e.unresolved())
	}
	rep := e.orphans(k.Kagent.(*kagent.Client))
	if len(rep.Deleted) != 1 || rep.Deleted[0] != landed.GetId() {
		t.Fatalf("orphan sweep deleted %v, want the old-Agent Session %s", rep.Deleted, landed.GetId())
	}
	if s, ok := e.srv.Session(landed.GetId()); ok && s.GetState() != kagentapi.RuntimeState_RUNTIME_STATE_DELETED {
		t.Fatalf("the old-Agent Session is still %s", s.GetState())
	}
	e.waitState(e.send("after the sweeps"), dbstate.DeliveryCompleted)
	if got := e.bindingKsid(m.SessionID); got != ksid {
		t.Fatalf("rebound to %q after the sweeps, want %q", got, ksid)
	}
}

// The main Session is bound on the old Agent, the configuration is renamed, then kagent drops the
// Session out of band. The replacement made inside the next delivery's turn is on the renamed Agent.
func TestAReplacementSessionInTheTurnIsOnTheRenamedAgent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	stop := e.run()
	e.waitState(e.send("one"), dbstate.DeliveryCompleted)
	stop()
	old := e.bindingRow()
	ksid := *old.KagentSessionID
	e.renameAgent("main-renamed")
	if _, err := e.other().DeleteSession(ctx, ksid); err != nil {
		t.Fatal(err)
	}
	e.run()
	e.waitState(e.send("two"), dbstate.DeliveryCompleted)
	now := e.bindingRow()
	if now.KagentSessionID == nil || *now.KagentSessionID == ksid {
		t.Fatalf("not replaced: %v", now.KagentSessionID)
	}
	sess, ok := e.srv.Session(*now.KagentSessionID)
	if !ok {
		t.Fatal("replacement missing")
	}
	if sess.GetAgent().GetNamespace() != e.svc.Agent.Namespace || sess.GetAgent().GetName() != "main-renamed" {
		t.Fatalf("replacement created on %s/%s, want main-renamed", sess.GetAgent().GetNamespace(), sess.GetAgent().GetName())
	}
}
