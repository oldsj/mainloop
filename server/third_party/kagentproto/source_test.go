package kagentproto

import (
	"os"
	"strings"
	"testing"
)

// deployedSourceRevision is the kagent revision the vendored protos are pinned to: the tip of
// oldsj/kagent mainloop-substrate-1-0 when it was vendored (see SOURCE).
// It is not yet a revision infra deploys. Update it, and SOURCE's commit, together when the fork
// moves, and when infra pins the ported fork in oldsj/infrastructure's
// k8s/apps/kagent/source-lock.yaml.
const deployedSourceRevision = "92061451411ad848c79ab52de6e955d8c4a487d7"

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
