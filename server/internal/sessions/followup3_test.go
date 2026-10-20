package sessions_test

import (
	"context"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/workspaces"
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

// A first-use main pins its Agent when its first create is requested. A create lands and its reply is
// lost, then the main Agent's configuration changes before the next send: the send replays the create
// on the pinned Agent and binds the Session that landed. Nothing is created on the renamed Agent and
// nothing leaks.
func TestAConfigChangeBetweenALostReplyAndTheNextSendBindsTheSessionThatLanded(t *testing.T) {
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
	// Three messages before any resolve pass: none fails, all bind the Session that landed.
	for _, text := range []string{"after the config change", "again, before the sweep", "and again"} {
		if d := e.waitState(e.send(text), dbstate.DeliveryCompleted); d.CreateFailures != 0 {
			t.Fatalf("%q: create failures = %d, want 0", text, d.CreateFailures)
		}
	}
	if ksid := e.bindingKsid(m.SessionID); ksid != landed.GetId() {
		t.Fatalf("bound %q, want the Session that landed, %q", ksid, landed.GetId())
	}
	if n := e.createsOn("main-renamed"); n != 0 {
		t.Fatalf("%d creates on the renamed Agent, want 0: the first-use main is pinned", n)
	}
	if err := e.svc.ResolveCreates(ctx); err != nil || e.unresolved() != 0 {
		t.Fatalf("ResolveCreates: %v, unresolved %d", err, e.unresolved())
	}
	if rep := e.orphans(k.Kagent.(*kagent.Client)); len(rep.Deleted) != 0 {
		t.Fatalf("orphan sweep deleted %v, want nothing: the landed Session is bound", rep.Deleted)
	}
}

// The main Session is bound on the first-use pinned Agent, the configuration is renamed, then kagent
// drops the Session out of band. The replacement made inside the next delivery's turn stays on the
// pinned Agent.
func TestAReplacementSessionInTheTurnStaysOnThePinnedAgent(t *testing.T) {
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
	if sess.GetAgent().GetNamespace() != e.svc.Agent.Namespace || sess.GetAgent().GetName() != "mainloop-main" {
		t.Fatalf("replacement created on %s/%s, want the pinned mainloop-main", sess.GetAgent().GetNamespace(), sess.GetAgent().GetName())
	}
}

// A first-use main pins the Agent of its first create. When kagent refuses that create before it
// reserved anything (the Agent named by a typo in the configuration is not prepared), the pin is
// dropped, so the corrected configuration is used by the next create.
func TestATypoInTheMainAgentIsNotPinnedOnceTheCreateIsRefused(t *testing.T) {
	e := newEnv(t)
	e.svc.Agent = kagent.AgentRef{Namespace: agent.Namespace, Name: "mainloop-mian"} // no such Agent
	stop := e.run()
	id := e.send("hello")
	e.waitFor("the create was refused and the pin dropped", func() bool {
		d := e.delivery(id)
		b := e.bindingRow()
		return d.CreateFailures > 0 && b.KagentAgentName == nil
	})
	if n := e.liveSessionsOn("mainloop-mian"); n != 0 {
		t.Fatalf("%d Sessions on the typo'd Agent", n)
	}
	// The configuration is fixed: the next create runs on the fixed Agent and pins it.
	e.svc.Agent = agent
	d := e.waitState(id, dbstate.DeliveryCompleted)
	stop()
	b := e.bindingRow()
	if b.KagentAgentName == nil || *b.KagentAgentName != agent.Name || b.KagentSessionID == nil {
		t.Fatalf("pin = %v, session %v, want the fixed Agent and a Session (delivery %s)", b.KagentAgentName, b.KagentSessionID, d.State)
	}
	if n := e.liveSessionsOn(agent.Name); n != 1 {
		t.Fatalf("%d Sessions on the fixed Agent, want 1", n)
	}
}
