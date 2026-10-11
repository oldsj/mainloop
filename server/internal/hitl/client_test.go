package hitl_test

import (
	"context"
	"testing"

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
