package hitl_test

import (
	"context"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/hitl"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
)

// The production sender, against the in-process fake kagent: the answer goes out once, as a user
// message to the waiting task with the HITL extension metadata, and the HITL header is on the call.
func TestProductionSenderPutsTheAnswerInTheExtensionMetadata(t *testing.T) {
	srv := kagenttest.New()
	t.Cleanup(srv.Close)
	client, err := kagent.New(kagent.Config{GatewayURL: srv.URL, UserID: "mainloop"})
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(s *hitl.Service) { s.Kagent = hitl.FromClient(client) })
	srv.AddTask(waitingTask("task-b", "msg-b", askRequest("q1")))
	e.observe()
	req := e.requestFor("task-b")
	e.run()

	if _, err := e.svc.Respond(context.Background(), req.ID, decision("act-q", api.AskUserResponse{
		Type: api.AskUserResponseTypeAskUserResponse, Id: "q1",
		Answers: []api.AskUserAnswer{{Answer: []string{"dev"}}},
	})); err != nil {
		t.Fatal(err)
	}
	r := e.waitResponse("act-q", dbstate.HITLAccepted)

	var sends []kagenttest.Request
	for _, rq := range srv.Requests() {
		if rq.Key == "SendStreamingMessage" {
			sends = append(sends, rq)
		}
	}
	if len(sends) != 1 {
		t.Fatalf("%d sends, want 1", len(sends))
	}
	s := sends[0]
	if s.MessageID != r.OutboundMessageID {
		t.Fatalf("message id %q, receipt %q", s.MessageID, r.OutboundMessageID)
	}
	if got := s.Header.Get("A2A-Extensions"); got != kagent.HITLExtension {
		t.Fatalf("A2A-Extensions %q", got)
	}
	if len(s.Extensions) != 1 || s.Extensions[0] != kagent.HITLExtension {
		t.Fatalf("message extensions %v", s.Extensions)
	}
	native, ok := s.Metadata[kagent.HITLExtension].(map[string]any)
	if !ok || native["type"] != "ask_user_response" || native["id"] != "q1" {
		t.Fatalf("metadata %v", s.Metadata)
	}

	task, err := client.GetTask(context.Background(), agent, "task-b")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status.State == a2a.TaskStateInputRequired {
		t.Fatalf("task still waits: %+v", task.Status)
	}
}

func testClient(t *testing.T) (*kagenttest.Server, *kagent.Client) {
	t.Helper()
	srv := kagenttest.New()
	t.Cleanup(srv.Close)
	client, err := kagent.New(kagent.Config{GatewayURL: srv.URL, UserID: "mainloop"})
	if err != nil {
		t.Fatal(err)
	}
	return srv, client
}

func sendOf() hitl.Send {
	return hitl.Send{
		MessageID: "msg-out", ContextID: ksid, TaskID: "task-b",
		Native: map[string]any{"type": "ask_user_response", "id": "q1"},
	}
}

// A refusal that is still being retried when the deadline passes ends as not sent, and the retry
// stops with it: nothing is delivered later.
func TestRefusalPastTheDeadlineIsNotSentAndNothingFollows(t *testing.T) {
	srv, client := testClient(t)
	for range 8 {
		srv.Script(kagenttest.Fail{Err: kagenttest.NotAccepted(60)})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	err := hitl.FromClient(client).SendHITL(ctx, agent, sendOf())
	if err == nil || !kagent.NotSent(err) {
		t.Fatalf("err = %v, want a send that provably did not leave", err)
	}
	atReturn := srv.Count("SendStreamingMessage")
	if atReturn == 0 {
		t.Fatal("the refusal was never tried")
	}
	time.Sleep(time.Second) // longer than any retry wait
	if n := srv.Count("SendStreamingMessage"); n != atReturn {
		t.Fatalf("%d sends at return, %d a second later: the retry kept running", atReturn, n)
	}
}

// A send that reached kagent and then hangs may have landed: it is not reported as not sent.
func TestASendThatMayHaveLandedIsNotNotSent(t *testing.T) {
	srv, client := testClient(t)
	srv.Script(kagenttest.Hang{})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := hitl.FromClient(client).SendHITL(ctx, agent, sendOf())
	if err == nil || kagent.NotSent(err) {
		t.Fatalf("err = %v, want an error that is not NotSent", err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := srv.Count("SendStreamingMessage"); n != 1 {
		t.Fatalf("%d sends, want exactly 1", n)
	}
}

// Through the service: the refusal outlasts SendTimeout, the response goes back to recorded and is
// sent again, and it is never held as uncertain.
func TestRefusalPastSendTimeoutIsReAnsweredNotUncertain(t *testing.T) {
	srv, client := testClient(t)
	for range 3 {
		srv.Script(kagenttest.Fail{Err: kagenttest.NotAccepted(150)})
	}
	e := newEnv(t, func(s *hitl.Service) {
		s.Kagent = hitl.FromClient(client)
		s.Opts.SendTimeout = 200 * time.Millisecond
	})
	srv.AddTask(waitingTask("task-b", "msg-b", askRequest("q1")))
	e.observe()
	req := e.requestFor("task-b")
	e.run()
	if _, err := e.svc.Respond(context.Background(), req.ID, decision("act-q", api.AskUserResponse{
		Type: api.AskUserResponseTypeAskUserResponse, Id: "q1",
		Answers: []api.AskUserAnswer{{Answer: []string{"dev"}}},
	})); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		r := e.response("act-q")
		if r.State == dbstate.HITLUncertain {
			t.Fatalf("the response went uncertain (detail %q)", deref(r.Detail))
		}
		if r.State == dbstate.HITLAccepted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("response is %s, want accepted", r.State)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
