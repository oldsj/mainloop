package sessions_test

import (
	"context"
	"errors"
	"testing"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/liveactor"
	"github.com/oldsj/mainloop/server/internal/sessions"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1"
)

// liveEnv is a main session on a live Agent whose actor is parked (idle-suspended): kagent reports
// no runtime association until the Session is resumed.
func liveEnv(t *testing.T) (e *env, mainID, ksid string) {
	t.Helper()
	e = newEnv(t, func(o *sessions.Options, _ *kagent.Config) { o.LiveAgents = []string{agent.Name} })
	e.srv.Actors, e.srv.IdleActors = true, true
	e.run()
	e.waitState(e.send("one"), dbstate.DeliveryCompleted)
	ksid = e.kagentSessionID()
	m, err := e.svc.EnsureMain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return e, m.SessionID, ksid
}

func TestLiveTargetResumesAStandaloneSessionDirectly(t *testing.T) {
	e, mainID, ksid := liveEnv(t)
	got, err := e.svc.LiveTarget(context.Background(), mainID)
	if err != nil {
		t.Fatal(err)
	}
	if want := (liveactor.Target{KagentSessionID: ksid, Actor: "ate/actor-" + ksid}); got != want {
		t.Fatalf("target %+v, want %+v", got, want)
	}
	if n := e.srv.Count(kagent.ProcResumeSession); n != 1 {
		t.Fatalf("%d resumes, want 1", n)
	}
	// Running now: no second resume.
	if _, err := e.svc.LiveTarget(context.Background(), mainID); err != nil || e.srv.Count(kagent.ProcResumeSession) != 1 {
		t.Fatalf("running: %v, %d resumes", err, e.srv.Count(kagent.ProcResumeSession))
	}
}

// addWorkspaceSession binds a workspace's session (id ws1, on the live Agent) to a new kagent Session,
// READY with its actor parked, and returns that Session's id.
func addWorkspaceSession(t *testing.T, e *env) string {
	t.Helper()
	made, err := e.other().CreateSession(context.Background(), &kagentapi.CreateSessionRequest{
		Agent: &kagentapi.ResourceReference{Namespace: agent.Namespace, Name: agent.Name}, RequestId: "ws1-create", Name: "ws1",
	})
	if err != nil {
		t.Fatal(err)
	}
	ksid := made.GetId()
	for _, q := range []string{
		`INSERT INTO sessions (id,user_id,title,description,prompt) SELECT 'ws1',user_id,title,description,prompt FROM sessions LIMIT 1`,
		`INSERT INTO projects (id,user_id,owner,name,full_name,html_url) VALUES ('p1','u','o','n','o/n','https://github.com/o/n')`,
		`INSERT INTO workspaces (id,user_id,project_id,repo_url,branch,agent_kind,agent_namespace,agent_name,request_id)
		    VALUES ('ws1','u','p1','https://github.com/o/n','b','claude','` + agent.Namespace + `','` + agent.Name + `','req1')`,
	} {
		if _, err := e.pool.Exec(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO agent_bindings (session_id,kind,role,kagent_session_id) VALUES ('ws1','claude','agent',$1)`, ksid); err != nil {
		t.Fatal(err)
	}
	return ksid
}

func TestLiveTargetWakesAWorkspaceSessionThroughTheWorkspaceService(t *testing.T) {
	e, _, _ := liveEnv(t)
	ksid := addWorkspaceSession(t, e)
	w := &wakeFake{resume: func() error {
		_, err := e.other().ResumeSession(context.Background(), ksid)
		return err
	}}
	e.svc.Waker = w
	got, err := e.svc.LiveTarget(context.Background(), "ws1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Actor != "ate/actor-"+ksid {
		t.Fatalf("actor %q", got.Actor)
	}
	if w.wakes.Load() != 1 {
		t.Fatalf("%d wakes, want 1", w.wakes.Load())
	}
	// Never resumed directly: the one resume is the wake's own.
	if n := e.srv.Count(kagent.ProcResumeSession); n != 1 {
		t.Fatalf("%d resumes (the wake's own)", n)
	}
}

func TestLiveTargetNeverResumesAWorkspaceSessionDirectly(t *testing.T) {
	e, _, _ := liveEnv(t)
	addWorkspaceSession(t, e)
	e.svc.Waker = nil
	if _, err := e.svc.LiveTarget(context.Background(), "ws1"); !errors.Is(err, liveactor.ErrWaking) {
		t.Fatalf("no waker: %v, want ErrWaking", err)
	}
	e.svc.Waker = &wakeFake{resume: func() error { return nil }} // wakes, but the actor stays parked
	if _, err := e.svc.LiveTarget(context.Background(), "ws1"); !errors.Is(err, liveactor.ErrWaking) {
		t.Fatalf("still parked: %v, want ErrWaking", err)
	}
	if n := e.srv.Count(kagent.ProcResumeSession); n != 0 {
		t.Fatalf("%d direct resumes, want 0", n)
	}
}

func TestLiveTargetRefusesSessionsThatAreNotLive(t *testing.T) {
	e, _, _ := liveEnv(t)
	if _, err := e.svc.LiveTarget(context.Background(), "missing"); !errors.Is(err, liveactor.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	// The same main session on an Agent that is not listed as live.
	off := newEnv(t)
	off.srv.Actors, off.srv.IdleActors = true, true
	off.run()
	off.waitState(off.send("one"), dbstate.DeliveryCompleted)
	m, err := off.svc.EnsureMain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := off.svc.LiveTarget(context.Background(), m.SessionID); !errors.Is(err, liveactor.ErrNotFound) {
		t.Fatalf("Agent not live: %v", err)
	}
	if n := e.srv.Count(kagent.ProcResumeSession) + off.srv.Count(kagent.ProcResumeSession); n != 0 {
		t.Fatalf("%d resumes for a session that is not live", n)
	}
}
