package mcpserver

import (
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/privileged"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

func TestTaskFailureTellsTheAgentHowToRecoverFromAMissingEnvironment(t *testing.T) {
	msg, ok := asToolError(taskFailure(&tasks.Error{Status: 409, Reason: tasks.ReasonEnvironmentRequired}))
	if !ok || !strings.HasPrefix(msg, "[409] project_environment_required") || !strings.Contains(msg, "select a development environment") {
		t.Fatalf("message = %q", msg)
	}
}

// An action the owner approved tells the agent how: from the web app, which carries no Touch ID
// signature, or on a phone with one.
func TestPrivilegedResultNamesTheApprovalChannel(t *testing.T) {
	for channel, want := range map[string]string{
		privileged.ChannelWeb:         "The owner approved it from the web app (no Touch ID).",
		privileged.ChannelPhoneSigned: "The owner approved it on their phone, signed with Touch ID.",
	} {
		c := channel
		r := privilegedResult(privileged.Action{ID: "pa-1", State: privileged.StateSucceeded, ApprovalChannel: &c}, nil)
		if !strings.Contains(r.Text, want) || r.Structured["approval_channel"] != channel {
			t.Errorf("%s: %q, %v", channel, r.Text, r.Structured["approval_channel"])
		}
	}
	r := privilegedResult(privileged.Action{ID: "pa-1", State: privileged.StateDenied}, nil)
	if _, ok := r.Structured["approval_channel"]; ok || strings.Contains(r.Text, "approved it") {
		t.Errorf("a denied action has a channel: %q", r.Text)
	}
}
