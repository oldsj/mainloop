package kagentproto

import (
	"os"
	"strings"
	"testing"
)

// deployedSourceRevision is oldsj/infrastructure's k8s/apps/kagent/source-lock.yaml
// controllerRuntime sourceRevision, as of infra PR #218. Update it, and SOURCE's commit, together
// when infra re-pins kagent.
const deployedSourceRevision = "a1164013675c2ad49ec4cc2b6b12d3dfa4a456d6"

// TestSourcePinnedToDeployedRevision fails if the vendored protos (and the generated clients built
// from them) drift from the kagent commit actually deployed, which SOURCE and this test must agree
// on by hand: nothing here can reach oldsj/infrastructure to check it live.
func TestSourcePinnedToDeployedRevision(t *testing.T) {
	raw, err := os.ReadFile("SOURCE")
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "commit: "
	var commit string
	for _, line := range strings.Split(string(raw), "\n") {
		if after, ok := strings.CutPrefix(line, prefix); ok {
			commit = strings.TrimSpace(after)
			break
		}
	}
	if commit == "" {
		t.Fatal("SOURCE has no commit: line")
	}
	if commit != deployedSourceRevision {
		t.Fatalf("SOURCE pins commit %s, but the deployed kagent revision is %s; refresh the vendored"+
			" protos (see SOURCE) and update both", commit, deployedSourceRevision)
	}
}
