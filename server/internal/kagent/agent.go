package kagent

import api "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1alpha1"

// AgentReady reports whether kagent can create Sessions on the Agent: its status names a
// latestSuccessfulRevision. That is the precondition CreateSession enforces ("Agent does not have a
// ready prepared revision"); a newer desired revision still compiling does not stop creates.
func AgentReady(a *api.Agent) bool {
	status := a.GetResource().GetValue().GetFields()["status"].GetStructValue()
	return status.GetFields()["latestSuccessfulRevision"].GetStringValue() != ""
}
