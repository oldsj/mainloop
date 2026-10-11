package sessions_test

import (
	"testing"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

func TestResolveAgentPrefersTheWorkspaceThenMainThenTheProvider(t *testing.T) {
	main := kagent.AgentRef{Namespace: "kagent", Name: "main"}
	s := &sessions.Service{Agent: main}
	ns, name, kind := "kagent", "claude-ws", "claude"

	got, err := s.ResolveAgent(&ns, &name, &kind, dbstate.RoleMain)
	if err != nil || got != (kagent.AgentRef{Namespace: ns, Name: name}) {
		t.Fatalf("workspace session = %+v, %v", got, err)
	}
	got, err = s.ResolveAgent(nil, nil, &kind, dbstate.RoleMain)
	if err != nil || got != main {
		t.Fatalf("main session = %+v, %v", got, err)
	}
	if _, err := s.ResolveAgent(nil, nil, &kind, dbstate.BindingRole("agent")); err == nil {
		t.Fatal("a session with no workspace and no provider must not resolve")
	}
}
