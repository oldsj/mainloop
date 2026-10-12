package mcpserver

import (
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/tasks"
)

func TestTaskFailureTellsTheAgentHowToRecoverFromAMissingEnvironment(t *testing.T) {
	msg, ok := asToolError(taskFailure(&tasks.Error{Status: 409, Reason: tasks.ReasonEnvironmentRequired}))
	if !ok || !strings.HasPrefix(msg, "[409] project_environment_required") || !strings.Contains(msg, "select a development environment") {
		t.Fatalf("message = %q", msg)
	}
}
