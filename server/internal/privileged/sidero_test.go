package privileged_test

import (
	"context"
	"errors"
	"slices"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/privileged"
)

// runKeyFixture stands for the run key a tool holds: the result must never carry it.
const runKeyFixture = "c2EtZmljdGlvbmFsLXJ1bi1rZXktOWQ0ZQ=="

// fakeSidero answers omnictl and talosctl runs from handle; runs keeps every run, dry runs
// included.
type fakeSidero struct {
	runs   []privileged.SideroRun
	handle func(run privileged.SideroRun) privileged.CommandOutput
	// upgrade answers CheckUpgrade; upgrades records its calls.
	upgrade  error
	upgrades []string
}

func (f *fakeSidero) CheckUpgrade(_ context.Context, cluster string, nodes []string, image string) error {
	f.upgrades = append(f.upgrades, cluster+" "+strings.Join(nodes, ",")+" "+image)
	return f.upgrade
}

func (f *fakeSidero) Run(_ context.Context, run privileged.SideroRun) (privileged.CommandOutput, error) {
	f.runs = append(f.runs, run)
	if run.Preflight != run.DryRun {
		return privileged.CommandOutput{}, fmt.Errorf("preflight %v but dry run %v: a preview only reads", run.Preflight, run.DryRun)
	}
	out := f.handle(run)
	out.Credentials = []string{runKeyFixture}
	return out, nil
}

func (f *fakeSidero) real() int {
	n := 0
	for _, r := range f.runs {
		if !r.Preflight {
			n++
		}
	}
	return n
}

func newSideroEnv(t *testing.T, handle func(run privileged.SideroRun) privileged.CommandOutput) (*kubeEnv, *fakeSidero) {
	t.Helper()
	e := newKubeEnv(t)
	e.svc.OmniOn = true
	f := &fakeSidero{handle: handle}
	e.ex.Sidero, e.ex.OmniClusters = f, []string{cluster}
	return e, f
}

const patchYAML = "metadata:\n  namespace: default\n  type: MachineSets.omni.sidero.dev\n  id: test-cluster-workers\n  labels:\n    omni.sidero.dev/cluster: test-cluster\n    omni.sidero.dev/role-worker: \"\"\nspec:\n  machineallocation:\n    name: widgets\n    machinecount: 3\n"

// applyPatch is omnictl apply of a machine set: its dry run prints a diff; the real run applies.
func applyPatch(run privileged.SideroRun) privileged.CommandOutput {
	if run.DryRun {
		return privileged.CommandOutput{Stdout: []byte("Processing 1 resource file(s)\n+    machinecount: 3\n")}
	}
	return privileged.CommandOutput{Stdout: []byte("Applied MachineSets.omni.sidero.dev(default/test-cluster-workers)\nkey " + runKeyFixture + "\n")}
}

func omnictlArgs(argv ...string) map[string]any {
	return map[string]any{"argv": append([]string{"omnictl"}, argv...)}
}

func omnictlApply(stdin string) map[string]any {
	return map[string]any{"argv": []string{"omnictl", "apply", "--file=-"}, "stdin": stdin}
}

func talosctlArgs(argv ...string) map[string]any {
	return map[string]any{"target": cluster, "argv": append([]string{"talosctl"}, argv...)}
}

// Given an Omni machine set, when main requests omnictl apply, then the card shows the command,
// the resources and the tool's dry-run diff, and the agent's facts leave the diff out. After an
// Approve the executor runs the dry run again, then the command once, and returns its output
// without the run's key.
func TestOmnictlApplyShowsTheDryRunAndRunsOnce(t *testing.T) {
	e, f := newSideroEnv(t, applyPatch)
	args := omnictlArgs("apply", "--file=-")
	args["stdin"] = patchYAML
	a := e.request(t, privileged.TypeOmnictl, "omni-1", args)
	a, card := e.preflighted(t, a.ID)
	cardHas(t, card, "Run omnictl apply through Omni", "omnictl apply --file=-", "test-cluster-workers",
		"the executor's proxy holds this to test-cluster: it changes only their resources",
		"Its dry run, which only reads, says:", "+    machinecount: 3", "runs the dry run again",
		"Risk: creates or updates Omni resources, Omni Admin.")
	if seen := string(privileged.AgentFacts(a.Type, a.Facts)); strings.Contains(seen, "machinecount") || !strings.Contains(seen, "owner's card only") {
		t.Errorf("the agent sees %s", seen)
	}
	if f.real() != 0 || len(f.runs) != 1 || f.runs[0].Stdin != patchYAML {
		t.Fatalf("preflight runs = %+v", f.runs)
	}
	e.approve(t, a.ID)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded {
		t.Fatalf("after execution: %+v (%v)", a, deref(a.LastError))
	}
	if f.real() != 1 || len(f.runs) != 3 {
		t.Fatalf("runs = %+v", f.runs)
	}
	res := kubectlResult(t, a)
	if !strings.Contains(res.Stdout, "Applied MachineSets") || strings.Contains(res.Stdout, runKeyFixture) {
		t.Fatalf("result = %+v", res)
	}
}

// Given an approved apply whose dry run now differs, the executor runs nothing.
func TestOmnictlIsStaleWhenTheDryRunDiffers(t *testing.T) {
	moved := false
	e, f := newSideroEnv(t, func(run privileged.SideroRun) privileged.CommandOutput {
		if moved {
			return privileged.CommandOutput{Stdout: []byte("+    machinecount: 1\n")}
		}
		return applyPatch(run)
	})
	args := omnictlArgs("apply", "--file=-")
	args["stdin"] = patchYAML
	a := e.request(t, privileged.TypeOmnictl, "omni-1", args)
	a, _ = e.preflighted(t, a.ID)
	e.approve(t, a.ID)
	moved = true
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateStale || f.real() != 0 {
		t.Fatalf("action = %+v (%v), real runs %d", a, deref(a.LastError), f.real())
	}
}

// A talosctl reboot has no preview: preflight runs nothing, the card says so, and the command runs
// once against the target cluster, never twice.
func TestTalosctlRebootRunsOnceWithoutPreview(t *testing.T) {
	e, f := newSideroEnv(t, func(privileged.SideroRun) privileged.CommandOutput {
		return privileged.CommandOutput{Stdout: []byte("reboot requested\n")}
	})
	a := e.request(t, privileged.TypeTalosctl, "talos-1", talosctlArgs("reboot", "--nodes=192.0.2.11", "--wait=false"))
	a, card := e.preflighted(t, a.ID)
	cardHas(t, card, "Run talosctl reboot on 192.0.2.11 (test-cluster) through Omni", "talosctl reboot --nodes=192.0.2.11 --wait=false",
		"only to nodes that Omni lists as machines of test-cluster", "Nothing can preview it", "Risk: reboots the node, no preview, Omni Admin.")
	if len(f.runs) != 0 {
		t.Fatalf("preflight ran %+v", f.runs)
	}
	e.approve(t, a.ID)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded || f.real() != 1 || f.runs[0].Cluster != cluster || f.runs[0].Tool != privileged.ToolTalosctl {
		t.Fatalf("action = %+v (%v), runs %+v", a, deref(a.LastError), f.runs)
	}

	again := e.request(t, privileged.TypeTalosctl, "talos-2", talosctlArgs("reboot", "--nodes=192.0.2.12", "--wait=false"))
	again, _ = e.preflighted(t, again.ID)
	e.approve(t, again.ID)
	e.exec(t, `INSERT INTO privileged_action_events (action_id, step, detail) VALUES ($1, 'command_started', 'talosctl reboot')`, again.ID)
	e.runExecutor(t)
	if again = e.get(t, again.ID); again.State != privileged.StateUncertain || f.real() != 1 {
		t.Fatalf("action = %+v (%v), real runs %d", again, deref(again.LastError), f.real())
	}
}

// An upgrade to an image the requester names says so on the card: the image runs as the node's
// operating system.
func TestTalosctlUpgradeImageIsTagged(t *testing.T) {
	e, f := newSideroEnv(t, applyPatch)
	image := "factory.talos.dev/metal-installer/" + strings.Repeat("ab", 32) + ":v1.14.5"
	a := e.request(t, privileged.TypeTalosctl, "up-1", talosctlArgs("upgrade", "--nodes=192.0.2.11,node-b", "--wait=false", "--image="+image))
	_, card := e.preflighted(t, a.ID)
	cardHas(t, card, "Risk: installs "+image+" as the node's operating system, which then has full control of the node, and reboots it")
	if want := []string{"test-cluster 192.0.2.11,node-b " + image}; !slices.Equal(f.upgrades, want) {
		t.Fatalf("upgrade checks = %q, want %q", f.upgrades, want)
	}
}

// An upgrade the proxy would refuse is refused before the card, with the proxy's reason.
func TestTalosctlDoomedUpgradeIsRefusedAtPreflight(t *testing.T) {
	e, f := newSideroEnv(t, applyPatch)
	f.upgrade = privileged.Refusal("the image's Talos v1.14.1 is older than machine m-1's v1.14.2: no downgrades (talosctl rollback goes back)")
	image := "factory.talos.dev/metal-installer/" + strings.Repeat("ab", 32) + ":v1.14.1"
	a := e.request(t, privileged.TypeTalosctl, "up-1", talosctlArgs("upgrade", "--nodes=192.0.2.11", "--wait=false", "--image="+image))
	e.runExecutor(t)
	e.run(t)
	if a = e.get(t, a.ID); a.State != privileged.StatePreflightFailed || !strings.Contains(deref(a.LastError), "no downgrades") {
		t.Fatalf("action = %+v (%v)", a, deref(a.LastError))
	}
	if f.real() != 0 || len(e.openItems(t)) != 0 {
		t.Fatalf("runs = %+v", f.runs)
	}
}

// Deletes, resets and shutdowns expire in an hour; other commands as any undecided action.
func TestSideroDestructiveCommandsExpireInAnHour(t *testing.T) {
	for _, tc := range []struct {
		typ  string
		args map[string]any
		want time.Duration
	}{
		{privileged.TypeTalosctl, talosctlArgs("reset", "--nodes=192.0.2.11", "--wait=false", "--graceful=false"), privileged.DeleteExpiry},
		{privileged.TypeTalosctl, talosctlArgs("shutdown", "--nodes=192.0.2.11", "--wait=false"), privileged.DeleteExpiry},
		{privileged.TypeOmnictl, omnictlArgs("delete", "machinesets", "test-cluster-workers"), privileged.DeleteExpiry},
		{privileged.TypeTalosctl, talosctlArgs("etcd", "remove-member", "--nodes=192.0.2.11", "3a1f"), privileged.DeleteExpiry},
		{privileged.TypeTalosctl, talosctlArgs("get", "members", "--nodes=192.0.2.11"), privileged.UndecidedExpiry},
		{privileged.TypeTalosctl, talosctlArgs("reboot", "--nodes=192.0.2.11", "--wait=false"), privileged.UndecidedExpiry},
	} {
		e, _ := newSideroEnv(t, applyPatch)
		a := e.request(t, tc.typ, "x1", tc.args)
		if got := a.ExpiresAt.Sub(a.CreatedAt).Round(time.Minute); got != tc.want {
			t.Errorf("%v: expires after %v, want %v", tc.args["argv"], got, tc.want)
		}
	}
}

// Requests the executor never runs are refused before anything reads Omni.
func TestSideroRequestRefusals(t *testing.T) {
	e, f := newSideroEnv(t, applyPatch)
	for i, tc := range []struct {
		typ  string
		args map[string]any
		want string
	}{
		{privileged.TypeOmnictl, omnictlArgs("kubeconfig", "internal-01"), "omnictl kubeconfig is not a command the executor runs"},
		{privileged.TypeOmnictl, omnictlArgs("talosconfig"), "is not a command the executor runs"},
		{privileged.TypeOmnictl, omnictlArgs("serviceaccount", "create", "x"), "is not a command the executor runs"},
		{privileged.TypeOmnictl, omnictlArgs("get", "clustersecrets"), "may hold secrets"},
		{privileged.TypeOmnictl, omnictlArgs("get", "Identities.omni.sidero.dev"), "may hold secrets"},
		{privileged.TypeOmnictl, omnictlArgs("get", "clusters", "--omniconfig=/tmp/x"), "--omniconfig is refused"},
		{privileged.TypeOmnictl, omnictlArgs("get", "clusters", "--insecure-skip-tls-verify"), "--insecure-skip-tls-verify is refused"},
		{privileged.TypeOmnictl, omnictlArgs("get", "clusters", "-w"), "short flags are refused"},
		{privileged.TypeOmnictl, omnictlArgs("get", "clusters", "--watch"), "--watch is refused: it never ends"},
		{privileged.TypeOmnictl, omnictlArgs("apply", "--file=/etc/passwd"), "needs --file=-"},
		{privileged.TypeOmnictl, omnictlArgs("apply", "--file=-", "--dry-run"), "--dry-run is refused"},
		{privileged.TypeOmnictl, omnictlArgs("machine-logs", "m1"), "omnictl machine-logs is not a command the executor runs"},
		{privileged.TypeTalosctl, talosctlArgs("get", "members"), "talosctl needs --nodes="},
		{privileged.TypeTalosctl, talosctlArgs("get", "members", "-n", "192.0.2.11"), "short flags are refused"},
		{privileged.TypeTalosctl, talosctlArgs("get", "members", "--nodes", "192.0.2.11"), "--nodes takes a value"},
		{privileged.TypeTalosctl, talosctlArgs("get", "machineconfig", "--nodes=192.0.2.11"), "may hold secrets"},
		{privileged.TypeTalosctl, talosctlArgs("get", "osrootsecrets", "--nodes=192.0.2.11"), "may hold secrets"},
		{privileged.TypeTalosctl, talosctlArgs("get", "members", "--nodes=192.0.2.11", "--namespace=secrets"), "the secrets namespace holds secrets"},
		{privileged.TypeTalosctl, talosctlArgs("read", "/system/secrets/x", "--nodes=192.0.2.11"), "talosctl read is not a command the executor runs"},
		{privileged.TypeTalosctl, talosctlArgs("kubeconfig", "--nodes=192.0.2.11"), "is not a command the executor runs"},
		{privileged.TypeTalosctl, talosctlArgs("etcd", "snapshot", "db", "--nodes=192.0.2.11"), "talosctl etcd is not a command the executor runs"},
		{privileged.TypeTalosctl, talosctlArgs("get", "members", "--nodes=192.0.2.11", "--talosconfig=/tmp/x"), "--talosconfig is refused"},
		{privileged.TypeTalosctl, talosctlArgs("get", "members", "--nodes=192.0.2.11", "--endpoints=10.0.0.1"), "--endpoints is refused"},
		{privileged.TypeTalosctl, talosctlArgs("get", "members", "--nodes=192.0.2.11", "--cluster=prod"), "--cluster is refused"},
		{privileged.TypeTalosctl, talosctlArgs("get", "members", "--nodes=192.0.2.11", "--context=x"), "--context is refused"},
		{privileged.TypeTalosctl, talosctlArgs("get", "members", "--nodes=192.0.2.11", "--", "--talosconfig=/x"), `"--" is refused`},
		{privileged.TypeTalosctl, talosctlArgs("get", "members", "--nodes=192.0.2.11", "--Namespace=x"), "not a flag name"},
		{privileged.TypeTalosctl, talosctlArgs("get", "members", "--nodes=192.0.2.11", "--output_x=y"), "not a flag name"},
		{privileged.TypeTalosctl, talosctlArgs("get", "members", "--nodes=192.0.2.11", "--output=go-template"), "--output takes table, yaml, json"},
		{privileged.TypeTalosctl, talosctlArgs("get", "members", "--nodes=192.0.2.11", "--namespace=-x"), `starting with "-" is refused`},
		{privileged.TypeTalosctl, talosctlArgs("get", "members", "--nodes=192.0.2.11", "--nodes=192.0.2.12"), "--nodes is given twice"},
		{privileged.TypeTalosctl, talosctlArgs("get", "members", "--nodes=192.0.2.11;x"), "not a node address"},
		{privileged.TypeTalosctl, talosctlArgs("dmesg", "--nodes=192.0.2.11"), "talosctl dmesg is not a command the executor runs"},
		{privileged.TypeTalosctl, talosctlArgs("logs", "machined", "--nodes=192.0.2.11", "--tail"), "--tail takes a value"},
		{privileged.TypeOmnictl, omnictlArgs("get", "machinestatuses"), "is not a resource type the executor reads"},
		{privileged.TypeOmnictl, omnictlArgs("get", "kernelargs"), "is not a resource type the executor reads"},
		{privileged.TypeOmnictl, omnictlArgs("get", "kernelargsstatuses"), "is not a resource type the executor reads"},
		{privileged.TypeOmnictl, omnictlArgs("get", "machinependingupdates"), "is not a resource type the executor reads"},
		{privileged.TypeTalosctl, talosctlArgs("reboot", "--nodes=192.0.2.11"), "needs --wait=false"},
		{privileged.TypeTalosctl, talosctlArgs("reboot", "--nodes=192.0.2.11", "--wait"), "needs --wait=false"},
		{privileged.TypeTalosctl, talosctlArgs("upgrade", "--nodes=192.0.2.11", "--wait=false", "--debug"), "--debug is refused"},
		{privileged.TypeTalosctl, talosctlArgs("service", "kubelet", "kill", "--nodes=192.0.2.11"), "takes start, stop, restart or status"},
		{privileged.TypeTalosctl, talosctlArgs("logs", "--nodes=192.0.2.11"), "takes 1 to 1 words"},
		{privileged.TypeTalosctl, talosctlArgs("get", "members\u202e", "--nodes=192.0.2.11"), "control or invisible"},
		{privileged.TypeTalosctl, talosctlArgs("get", "members", "--nodes=192.0.2.11", "--namespace=password=hunter2hunter2"), "looks like a credential"},
		// Only the types the executor reads, by their full names; the review's secret-bearing
		// types among the refused.
		{privileged.TypeOmnictl, omnictlArgs("get", "configpatches"), "configpatches is not a resource type the executor reads"},
		{privileged.TypeOmnictl, omnictlArgs("get", "ConfigPatches.omni.sidero.dev"), "is not a resource type the executor reads"},
		{privileged.TypeOmnictl, omnictlArgs("get", "siderolinkconfigs"), "is not a resource type the executor reads"},
		{privileged.TypeOmnictl, omnictlArgs("get", "redactedclustermachineconfigs"), "is not a resource type the executor reads"},
		{privileged.TypeTalosctl, talosctlArgs("get", "linkspecs", "--nodes=192.0.2.11"), "is not a resource type the executor reads"},
		{privileged.TypeTalosctl, talosctlArgs("get", "registryconfigs", "--nodes=192.0.2.11"), "is not a resource type the executor reads"},
		{privileged.TypeTalosctl, talosctlArgs("get", "manifests", "--nodes=192.0.2.11"), "is not a resource type the executor reads"},
		{privileged.TypeTalosctl, talosctlArgs("get", "kubespanconfigs", "--nodes=192.0.2.11"), "is not a resource type the executor reads"},
		{privileged.TypeTalosctl, talosctlArgs("get", "discoveryconfigs", "--nodes=192.0.2.11"), "is not a resource type the executor reads"},
		{privileged.TypeTalosctl, talosctlArgs("get", "volumeconfigs", "--nodes=192.0.2.11"), "is not a resource type the executor reads"},
		{privileged.TypeTalosctl, talosctlArgs("get", "extensionserviceconfigs", "--nodes=192.0.2.11"), "is not a resource type the executor reads"},
		{privileged.TypeTalosctl, talosctlArgs("get", "siderolinkconfigs", "--nodes=192.0.2.11"), "is not a resource type the executor reads"},
		{privileged.TypeTalosctl, talosctlArgs("get", "mc", "--nodes=192.0.2.11"), "is not a resource type the executor reads"},
		{privileged.TypeTalosctl, talosctlArgs("get", "clusters", "--nodes=192.0.2.11"), "is not a resource type the executor reads"},
		{privileged.TypeOmnictl, omnictlArgs("delete", "machinesets"), "takes 2 to 2 words"},
		{privileged.TypeOmnictl, omnictlArgs("delete", "machinesets", "--selector=omni.sidero.dev/cluster=test-cluster"), "does not take --selector"},
		{privileged.TypeTalosctl, talosctlArgs("upgrade", "--nodes=192.0.2.11", "--wait=false", "--image=docker.io/someone/installer:v1.14.5"),
			"--image takes an installer image with its version"},
		{privileged.TypeTalosctl, talosctlArgs("upgrade", "--nodes=192.0.2.11", "--wait=false", "--image=ghcr.io/siderolabs.example.com/installer:v1"),
			"--image takes an installer image"},
		{privileged.TypeTalosctl, talosctlArgs("upgrade", "--nodes=192.0.2.11", "--wait=false", "--image=ghcr.io/siderolabs/../evil/installer"),
			"--image takes an installer image"},
		{privileged.TypeTalosctl, talosctlArgs("upgrade", "--nodes=192.0.2.11", "--wait=false", "--image=factory.talos.dev/metal-installer/" + strings.Repeat("ab", 32)),
			"--image takes an installer image with its version"},
		{privileged.TypeTalosctl, talosctlArgs("upgrade", "--nodes=192.0.2.11", "--wait=false", "--image=ghcr.io/siderolabs/talosctl:v1.14.5"),
			"--image takes an installer image with its version"},
		{privileged.TypeTalosctl, talosctlArgs("upgrade", "--nodes=192.0.2.11", "--wait=false",
			"--image=factory.talos.dev/metal-installer/"+strings.Repeat("ab", 32)+":v1.14.5@sha256:"+strings.Repeat("0", 64)),
			"--image takes an installer image with its version tag and no digest"},
		{privileged.TypeTalosctl, talosctlArgs("upgrade", "--nodes=192.0.2.11", "--wait=false"), "talosctl upgrade needs --image="},
	} {
		_, err := e.svc.Request(context.Background(), mainSession, tc.typ, jsonArgs(tc.args), "r", fmt.Sprintf("r%d", i))
		if !errors.Is(err, privileged.ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.args["argv"], err, tc.want)
		}
	}
	for i, tc := range []struct {
		typ  string
		args map[string]any
	}{
		{privileged.TypeTalosctl, map[string]any{"argv": []string{"talosctl", "version", "--nodes=n1"}}},
		{privileged.TypeOmnictl, map[string]any{"target": cluster, "argv": []string{"omnictl", "get", "clusters"}}},
		{privileged.TypeOmnictl, map[string]any{"argv": []string{"talosctl", "get", "clusters"}}},
		{privileged.TypeOmnictl, map[string]any{"argv": []string{"omnictl", "apply", "--file=-"}}},
		{privileged.TypeOmnictl, map[string]any{"argv": []string{"omnictl", "get", "clusters"}, "stdin": "x"}},
		{privileged.TypeOmnictl, map[string]any{"argv": []string{"omnictl", "apply", "--file=-"},
			"stdin": "metadata:\n  namespace: default\n  type: JoinTokens.omni.sidero.dev\n  id: x\n"}},
		{privileged.TypeOmnictl, map[string]any{"argv": []string{"omnictl", "apply", "--file=-"}, "stdin": "spec: {}\n"}},
		{privileged.TypeOmnictl, map[string]any{"argv": []string{"omnictl", "apply", "--file=-"},
			"stdin": "metadata:\n  namespace: default\n  type: ConfigPatches.omni.sidero.dev\n  id: x\n"}},
	} {
		if _, err := e.svc.Request(context.Background(), mainSession, tc.typ, jsonArgs(tc.args), "r", fmt.Sprintf("s%d", i)); !errors.Is(err, privileged.ErrInvalid) {
			t.Errorf("%v: err = %v", tc.args, err)
		}
	}
	if len(f.runs) != 0 {
		t.Fatalf("runs = %+v", f.runs)
	}
}

// Another cluster, and a dry run that fails, are refused at preflight with the reason only.
func TestSideroPreflightRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		typ    string
		args   map[string]any
		handle func(privileged.SideroRun) privileged.CommandOutput
		want   string
	}{
		"another cluster": {privileged.TypeTalosctl, map[string]any{"target": "prod-02", "argv": []string{"talosctl", "version", "--nodes=n1"}},
			applyPatch, `target "prod-02" is not a cluster this executor may act on`},
		"omnictl on another cluster": {privileged.TypeOmnictl, omnictlArgs("cluster", "status", "prod-02"), applyPatch,
			`cluster "prod-02" is not one this executor acts on (test-cluster)`},
		"apply to another cluster": {privileged.TypeOmnictl, omnictlApply(strings.ReplaceAll(patchYAML, "cluster: test-cluster", "cluster: prod-02")), applyPatch,
			`(MachineSets.omni.sidero.dev test-cluster-workers) belongs to cluster "prod-02"`},
		"apply to no cluster": {privileged.TypeOmnictl, omnictlApply("metadata:\n  namespace: default\n  type: MachineClasses.omni.sidero.dev\n  id: big\nspec: {}\n"), applyPatch,
			"(MachineClasses.omni.sidero.dev big) belongs to no cluster"},
		"dry run fails": {privileged.TypeOmnictl, omnictlArgs("cluster", "delete", "test-cluster"), func(privileged.SideroRun) privileged.CommandOutput {
			return privileged.CommandOutput{ExitCode: 1, Stderr: []byte("rpc error: code = NotFound desc = cluster widgets has secret things")}
		}, "the dry run of omnictl cluster delete failed (exit 1, NotFound); the executor's log has its message"},
		"dry run refused by the proxy": {privileged.TypeOmnictl, omnictlArgs("cluster", "delete", "test-cluster"), func(privileged.SideroRun) privileged.CommandOutput {
			return privileged.CommandOutput{ExitCode: 1, Stderr: []byte("rpc error: code = PermissionDenied desc = refused by the executor: /cosi.resource.State/Destroy writes")}
		}, "refused by the executor's proxy"},
	} {
		t.Run(name, func(t *testing.T) {
			e, f := newSideroEnv(t, tc.handle)
			a := e.request(t, tc.typ, "r1", tc.args)
			e.runExecutor(t)
			e.run(t)
			if a = e.get(t, a.ID); a.State != privileged.StatePreflightFailed || !strings.Contains(deref(a.LastError), tc.want) ||
				strings.Contains(deref(a.LastError), "secret things") {
				t.Fatalf("action = %+v (%v), want %q", a, deref(a.LastError), tc.want)
			}
			if f.real() != 0 || len(e.openItems(t)) != 0 {
				t.Fatalf("runs = %+v", f.runs)
			}
		})
	}
}

// Without Omni access, serve-api answers the Sidero types unavailable.
func TestSideroNeedsOmniAccess(t *testing.T) {
	e, _ := newSideroEnv(t, applyPatch)
	e.svc.OmniOn = false
	_, err := e.svc.Request(context.Background(), mainSession, privileged.TypeOmnictl, jsonArgs(omnictlArgs("get", "clusters")), "r", "r1")
	if !errors.Is(err, privileged.ErrUnsupported) || !strings.Contains(err.Error(), "Omni access") {
		t.Fatalf("err = %v", err)
	}
}
