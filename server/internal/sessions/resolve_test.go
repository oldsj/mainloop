package sessions_test

import (
	"testing"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

func TestResolveAgentPrefersTheExplicitReferenceThenMainThenTheProvider(t *testing.T) {
	t.Parallel()
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
	empty := ""
	got, err = s.ResolveAgent(&empty, &empty, &kind, dbstate.RoleMain)
	if err != nil || got != main {
		t.Fatalf("main session without explicit reference = %+v, %v", got, err)
	}
	if _, err := s.ResolveAgent(nil, nil, &kind, dbstate.BindingRole("agent")); err == nil {
		t.Fatal("a session with no workspace and no provider must not resolve")
	}
}

func TestResolveAgentAcceptsStoredReferenceWithoutAProvider(t *testing.T) {
	t.Parallel()
	s := &sessions.Service{Agent: kagent.AgentRef{Namespace: "kagent", Name: "main"}}
	ns, name, kind := "scheduled", "unnamed-agent", "scheduled"
	for _, role := range []dbstate.BindingRole{dbstate.RoleAgent, dbstate.RoleMain} {
		got, err := s.ResolveAgent(&ns, &name, &kind, role)
		if err != nil || got != (kagent.AgentRef{Namespace: ns, Name: name}) {
			t.Fatalf("stored Agent with role %s = %+v, %v", role, got, err)
		}
	}
}
