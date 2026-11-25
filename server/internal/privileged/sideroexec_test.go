package privileged

import (
	"bytes"
	"context"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/siderolabs/go-api-signature/pkg/pgp"
	"github.com/siderolabs/go-api-signature/pkg/serviceaccount"

	"github.com/oldsj/mainloop/server/internal/config"
)

// realSidero is the pinned omnictl or talosctl: MAINLOOP_TEST_OMNICTL or MAINLOOP_TEST_TALOSCTL
// (CI installs them there), or the tool on PATH when it is the same release. Without one the test
// skips; with the variable set, a different release fails it.
func realSidero(t *testing.T, tool string) string {
	t.Helper()
	bin, want, versionArgs := config.TestOmnictl(), OmnictlVersion, []string{"--version"}
	if tool == ToolTalosctl {
		bin, want, versionArgs = config.TestTalosctl(), TalosctlVersion, []string{"version", "--client", "--short"}
	}
	named := bin != ""
	if !named {
		var err error
		if bin, err = exec.LookPath(tool); err != nil {
			t.Skipf("no %s: set MAINLOOP_TEST_%s to the pinned binary", tool, strings.ToUpper(tool))
		}
	}
	out, err := exec.CommandContext(t.Context(), bin, versionArgs...).CombinedOutput()
	got := regexp.MustCompile(`v\d+\.\d+\.\d+`).FindString(string(out))
	switch {
	case (err != nil || got != want) && named:
		t.Fatalf("%s is %s %q (%v), not the pinned %s", bin, tool, got, err, want)
	case err != nil || got != want:
		t.Skipf("%s on PATH is %q, not the pinned %s", tool, got, want)
	}
	return bin
}

// Given the pinned binaries, when the executor runs one, then it reaches Omni only through the
// proxy: Omni sees calls signed by the executor's key, routed to the target cluster and node, and
// the binary held only the run's key.
func TestSideroRealBinaries(t *testing.T) {
	for _, tc := range []struct {
		tool   string
		argv   []string
		method string
		md     map[string]string
	}{
		{ToolOmnictl, []string{"omnictl", "get", "clusters"}, cosiState + "Get", nil},
		{ToolTalosctl, []string{"talosctl", "version", "--nodes=192.0.2.11"}, "/machine.MachineService/Version",
			map[string]string{"node": "192.0.2.11", "context": "internal-01"}},
		{ToolTalosctl, []string{"talosctl", "get", "members", "--nodes=192.0.2.11"}, cosiState + "List",
			map[string]string{"node": "192.0.2.11", "context": "internal-01"}},
	} {
		t.Run(strings.Join(tc.argv, " "), func(t *testing.T) {
			bin := realSidero(t, tc.tool)
			key, err := pgp.GenerateKey("mainloop-executor", "", "mainloop-executor@serviceaccount.omni.example", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			real := &serviceaccount.ServiceAccount{Name: "mainloop-executor@serviceaccount.omni.example", Key: key}
			realB64, err := serviceaccount.Encode(real.Name, key)
			if err != nil {
				t.Fatal(err)
			}
			omni, upstream := startFakeOmni(t, real)
			omni.reply = testWorld().reply
			e := ExecSidero{Endpoint: "https://omni.example.com", Key: real, Clusters: []string{"internal-01"}, Binaries: map[string]string{tc.tool: bin}, Dir: t.TempDir(), Upstream: upstream}
			out, err := e.Run(context.Background(), SideroRun{Tool: tc.tool, Cluster: "internal-01", Argv: tc.argv, Timeout: 30 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			calls := omni.seen()
			if !slices.ContainsFunc(calls, func(c omniCall) bool { return c.method == tc.method }) {
				t.Fatalf("Omni saw %+v; the tool said %s %s", calls, out.Stdout, out.Stderr)
			}
			machines := cosiList(omniDefaultNamespace, omniClusterMachineIDs)
			for _, c := range calls {
				if c.sigErr != nil {
					t.Errorf("%s: %v", c.method, c.sigErr)
				}
				if bytes.Equal(c.body, machines) {
					continue // the proxy's own lookup, of Omni
				}
				for k, v := range tc.md {
					if !slices.Equal(c.md.Get(k), []string{v}) {
						t.Errorf("%s: %s = %q, want %q", c.method, k, c.md.Get(k), v)
					}
				}
			}
			if len(out.Credentials) != 1 || out.Credentials[0] == realB64 || strings.Contains(string(out.Stdout)+string(out.Stderr), out.Credentials[0]) {
				t.Errorf("credentials = %d", len(out.Credentials))
			}
		})
	}
}
