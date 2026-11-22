package tasks_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/providers"
	"github.com/oldsj/mainloop/server/internal/sessions"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// mcpProcess is a task service built as the MCP process builds its own: a session sender and the
// kagent liveness reader, no workspace runtime and no loops. The rig's service plays the API
// process, which runs the loops on the same database.
func (r *rig) mcpProcess() *tasks.Service {
	r.t.Helper()
	cfg := mustConfig(r.t)
	reg := r.svc.Providers.(*providers.Registry) //nolint:forcetypeassert // newEnv always builds one
	sess := sessions.NewService(cfg, r.pool, nil, reg, nil, nil, nil, nil, slog.Default())
	sess.OwnerID = owner
	svc := tasks.NewService(cfg, r.pool, reg, slog.Default(), tasks.WithSender(sess),
		tasks.WithLivenessObserver(&tasks.KagentLiveness{Pool: r.pool, OwnerID: owner, Sessions: sess, Kagent: r.live}))
	svc.Limits = r.svc.Limits
	return svc
}

func TestTheMCPProcessSendsToAStandingSupervisor(t *testing.T) {
	r := newRig(t)
	r.run()
	id, session, _ := r.runningStanding()
	r.finishTurns()
	mcp := r.mcpProcess()
	main := r.mainPrincipal()

	// When main's supervisor_send is served by the MCP process
	res, err := mcp.SupervisorSend(context.Background(), main, tasks.SupervisorSendInput{ProjectID: "p1", RequestID: "s-2", Text: "second batch"})

	// Then the message is recorded for the supervisor's session, and the API process delivers it
	if err != nil {
		t.Fatalf("supervisor_send from the MCP process: %v", err)
	}
	if res.TaskID != id || res.SessionID != session {
		t.Fatalf("sent to %s/%s, want %s/%s", res.TaskID, res.SessionID, id, session)
	}
	r.eventually("message delivered", func() bool {
		return r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1 AND content LIKE '%second batch%'`, session) == 1
	})

	// And a retry of the same request sends nothing twice
	if _, err := mcp.SupervisorSend(context.Background(), main, tasks.SupervisorSendInput{ProjectID: "p1", RequestID: "s-2", Text: "second batch"}); err != nil {
		t.Fatal(err)
	}
	if n := r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1 AND content LIKE '%second batch%'`, session); n != 1 {
		t.Fatalf("messages = %d", n)
	}

	// And its task_follow_up to the supervisor works too
	if _, err := mcp.FollowUp(context.Background(), main, tasks.FollowUpInput{TaskID: id, RequestID: "f-1", Text: "third"}); err != nil {
		t.Fatalf("follow_up from the MCP process: %v", err)
	}
}

func TestTheMCPProcessRecoversAGoneSupervisorOnSend(t *testing.T) {
	r := newRig(t)
	r.run()
	id, session, _ := r.runningStanding()
	r.finishTurns()
	r.bindKagent(session, "ks-s")
	r.live.set("ks-s", kagent.SessionLiveness{})
	mcp := r.mcpProcess()

	// When main sends through the MCP process to a supervisor whose session is gone
	_, err := mcp.SupervisorSend(context.Background(), r.mainPrincipal(), tasks.SupervisorSendInput{ProjectID: "p1", RequestID: "s-2", Text: "second batch"})

	// Then it is told the supervisor is starting, and the API process makes the successor
	wantRefusal(t, err, 409, tasks.ReasonSupervisorStarting)
	r.handedOff(id)
	if _, s := r.attemptOf(id); s == session {
		t.Fatal("no successor session")
	}
}
