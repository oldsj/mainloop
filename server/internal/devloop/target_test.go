package devloop_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/devloop"
	"github.com/oldsj/mainloop/server/internal/devloop/devlooptest"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func seedProject(t *testing.T, d *testdb.DB, id, fullName string) {
	t.Helper()
	owner, name, _ := strings.Cut(fullName, "/")
	if _, err := d.Pool.Exec(context.Background(), `INSERT INTO projects (id, user_id, owner, name, full_name, html_url, default_branch)
		VALUES ($1, 'owner', $2, $3, $4, 'https://x', 'main') ON CONFLICT (id) DO NOTHING`, id, owner, name, fullName); err != nil {
		t.Fatal(err)
	}
}

func minimalTarget(projectID string) devloop.Target {
	return devloop.Target{
		Kind: devloop.TargetNamespace, Namespace: "ns", Workload: "app=x", CommandContainer: "main",
		SyncPaths: []devloop.SyncPath{{Selector: "app=x", Container: "main", Prefix: "server", Dest: "/src"}},
	}
}

// TestTargetServiceSetThenGetRoundTrips proves the behavior that fails without the change: a
// target written through the owner-only API is readable by project id, with every field intact,
// and gets its own lease and sync lock rows.
func TestTargetServiceSetThenGetRoundTrips(t *testing.T) {
	t.Parallel()
	d := testdb.New(t)
	seedProject(t, d, "p1", "o/n")
	svc := &devloop.TargetService{Pool: d.Pool}
	migrate := "make migrate"
	in := minimalTarget("p1")
	in.MigrateCmd = &migrate
	created, err := svc.Set(context.Background(), "p1", in)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.ProjectID != "p1" {
		t.Fatalf("Set = %+v, want an id and project_id p1", created)
	}
	got, err := svc.Get(context.Background(), "p1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != created.ID || got.Namespace != "ns" || got.Workload != "app=x" || got.CommandContainer != "main" ||
		len(got.SyncPaths) != 1 || got.SyncPaths[0].Prefix != "server" || got.MigrateCmd == nil || *got.MigrateCmd != migrate {
		t.Fatalf("Get after Set = %+v, want it to round-trip", got)
	}
	var leaseRows, lockRows int
	if err := d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM devloop_lease WHERE id = $1`, created.ID).Scan(&leaseRows); err != nil {
		t.Fatal(err)
	}
	if err := d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM devloop_sync_lock WHERE id = $1`, created.ID).Scan(&lockRows); err != nil {
		t.Fatal(err)
	}
	if leaseRows != 1 || lockRows != 1 {
		t.Fatalf("lease rows = %d, sync lock rows = %d, want one each for the new target", leaseRows, lockRows)
	}
}

// TestTargetServiceGetWithNoneConfiguredIsNotFound proves the project check this slice replaces
// the hardcoded oldsj/mainloop check with: a project with no target configured is reported as such.
func TestTargetServiceGetWithNoneConfiguredIsNotFound(t *testing.T) {
	t.Parallel()
	d := testdb.New(t)
	seedProject(t, d, "p1", "o/n")
	_, err := (&devloop.TargetService{Pool: d.Pool}).Get(context.Background(), "p1")
	if !errors.Is(err, devloop.ErrTargetNotFound) {
		t.Fatalf("Get with none configured = %v, want ErrTargetNotFound", err)
	}
}

// TestTargetServiceSetKeepsTheSameIDOnUpdate proves editing an existing target never orphans its
// lease or sync lock: a second Set for the same project reuses the first call's id.
func TestTargetServiceSetKeepsTheSameIDOnUpdate(t *testing.T) {
	t.Parallel()
	d := testdb.New(t)
	seedProject(t, d, "p1", "o/n")
	svc := &devloop.TargetService{Pool: d.Pool}
	first, err := svc.Set(context.Background(), "p1", minimalTarget("p1"))
	if err != nil {
		t.Fatal(err)
	}
	edited := minimalTarget("p1")
	edited.Namespace = "ns2"
	second, err := svc.Set(context.Background(), "p1", edited)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Namespace != "ns2" {
		t.Fatalf("second Set = %+v, want the same id %s with the new namespace", second, first.ID)
	}
}

// TestTargetServiceSetRejectsAnUnworkableTarget proves the validation this slice needs: a vcluster
// kind without its inner location, and a command pod that is not one of the sync paths, are both
// refused rather than silently accepted and failing only when a sync is attempted.
func TestTargetServiceSetRejectsAnUnworkableTarget(t *testing.T) {
	t.Parallel()
	d := testdb.New(t)
	seedProject(t, d, "p1", "o/n")
	svc := &devloop.TargetService{Pool: d.Pool}
	ctx := context.Background()

	vcluster := minimalTarget("p1")
	vcluster.Kind = devloop.TargetVCluster
	if _, err := svc.Set(ctx, "p1", vcluster); !errors.Is(err, devloop.ErrInvalidTarget) {
		t.Errorf("vcluster target with no vcluster_name/namespace = %v, want ErrInvalidTarget", err)
	}

	mismatched := minimalTarget("p1")
	mismatched.Workload = "app=other"
	if _, err := svc.Set(ctx, "p1", mismatched); !errors.Is(err, devloop.ErrInvalidTarget) {
		t.Errorf("workload matching no sync path = %v, want ErrInvalidTarget", err)
	}
}

// TestLeaseIsIsolatedPerTarget proves the core of this slice: two projects' targets each get their
// own lease, so one session holding one project's lease does not block another project's sync.
func TestLeaseIsIsolatedPerTarget(t *testing.T) {
	t.Parallel()
	d := testdb.New(t)
	seedProject(t, d, "p1", "o/n1")
	seedProject(t, d, "p2", "o/n2")
	targets := &devloop.TargetService{Pool: d.Pool}
	a, err := targets.Set(context.Background(), "p1", minimalTarget("p1"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := targets.Set(context.Background(), "p2", minimalTarget("p2"))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatalf("two different projects' targets share an id: %s", a.ID)
	}
	lease := &devloop.LeaseService{Pool: d.Pool}
	ctx := context.Background()
	if info, err := lease.Acquire(ctx, a.ID, "session-a"); err != nil || info.Status != devloop.LeaseAcquired {
		t.Fatalf("Acquire(a) = %+v, %v, want acquired", info, err)
	}
	// session-b asks for the OTHER project's lease: it must be granted at once, not queued behind
	// session-a, which holds a different target's lease entirely.
	if info, err := lease.Acquire(ctx, b.ID, "session-b"); err != nil || info.Status != devloop.LeaseAcquired {
		t.Fatalf("Acquire(b) while a holds a's lease = %+v, %v, want acquired (independent targets)", info, err)
	}
	if held, err := lease.Holds(ctx, a.ID, "session-a"); err != nil || !held {
		t.Errorf("session-a holds a's lease = %v, %v, want true", held, err)
	}
	if held, err := lease.Holds(ctx, b.ID, "session-a"); err != nil || held {
		t.Errorf("session-a holds b's lease = %v, %v, want false", held, err)
	}
}

// TestMigrationSeedsOldsjMainloopsTarget proves migration 00310_devloop_targets.sql's seed: when a
// project named oldsj/mainloop exists, it is given the same target mainloop-dev's own lease and
// sync lock rows (seeded by the original devloop migration) already use, so dev_sync for Mainloop
// keeps working unchanged. The INSERT mirrors the migration file; testdb's template is migrated
// before any project exists, so the migration's own run seeds nothing there, and this is the
// smallest way to prove its SELECT ... WHERE full_name logic without reimplementing goose's partial
// replay.
func TestMigrationSeedsOldsjMainloopsTarget(t *testing.T) {
	t.Parallel()
	d := testdb.New(t)
	seedProject(t, d, "mainloop-project", "oldsj/mainloop")
	const seedSQL = `
INSERT INTO devloop_targets (
    id, project_id, kind, namespace, workload, command_container, sync_paths,
    migrate_cmd, health_cmd, smoke_cmd
)
SELECT
    'mainloop-dev', p.id, 'namespace', 'mainloop-dev', 'app=mainloop-backend', 'backend',
    '[{"selector": "app=mainloop-backend", "container": "backend", "prefix": "server", "dest": "/src"}]'::jsonb,
    'go run ./cmd/mainloop migrate up', 'curl -fsS http://127.0.0.1:8000/health',
    'go run ./cmd/mainloop smoke --read-only'
FROM projects p
WHERE lower(p.full_name) = 'oldsj/mainloop'
ORDER BY p.created_at
LIMIT 1
ON CONFLICT (id) DO NOTHING`
	if _, err := d.Pool.Exec(context.Background(), seedSQL); err != nil {
		t.Fatal(err)
	}
	target, err := (&devloop.TargetService{Pool: d.Pool}).Get(context.Background(), "mainloop-project")
	if err != nil {
		t.Fatal(err)
	}
	if target.ID != devloop.LeaseID || target.Namespace != "mainloop-dev" || target.Kind != devloop.TargetNamespace {
		t.Fatalf("seeded target = %+v, want id %q in namespace mainloop-dev", target, devloop.LeaseID)
	}
	// mainloop-dev's lease and sync lock rows, seeded by the original devloop migration under this
	// same id, must already exist: dev_sync for Mainloop needs no new row for them.
	var leaseRows, lockRows int
	if err := d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM devloop_lease WHERE id = $1`, devloop.LeaseID).Scan(&leaseRows); err != nil {
		t.Fatal(err)
	}
	if err := d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM devloop_sync_lock WHERE id = $1`, devloop.LeaseID).Scan(&lockRows); err != nil {
		t.Fatal(err)
	}
	if leaseRows != 1 || lockRows != 1 {
		t.Fatalf("lease rows = %d, sync lock rows = %d, want the one each seeded by 00250_devloop.sql", leaseRows, lockRows)
	}
}

// fakeSecretReader answers ReadSecretKey from an in-memory map, or Err if set.
type fakeSecretReader struct {
	data map[string][]byte
	err  error
}

func (f *fakeSecretReader) ReadSecretKey(_ context.Context, namespace, name, key string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	v, ok := f.data[namespace+"/"+name+"/"+key]
	if !ok {
		return nil, errors.New("fakeSecretReader: no such key")
	}
	return v, nil
}

// minimalKubeconfig parses successfully (clientcmd never dials); it is enough to prove the
// vcluster kind builds a client scoped to the right inner namespace.
const minimalKubeconfig = `
apiVersion: v1
kind: Config
clusters:
- cluster: {server: https://vcluster.example:6443}
  name: vc
contexts:
- context: {cluster: vc, user: vc}
  name: vc
current-context: vc
users:
- name: vc
  user: {token: faketoken}
`

func vclusterTarget() devloop.Target {
	return devloop.Target{
		Kind: devloop.TargetVCluster, Namespace: "kagent-staging", VClusterName: "kv", VClusterNamespace: "kagent-dev",
		Workload: "app=kagent-controller", CommandContainer: "controller",
		SyncPaths: []devloop.SyncPath{{Selector: "app=kagent-controller", Container: "controller", Prefix: "server", Dest: "/src"}},
	}
}

// TestVClusterTargetFailsClearlyWithoutAConfiguredFactory proves the gate this slice requires:
// until a VClusterClientFactory is wired in production (the infra grant in docs/specs/devloop.md
// is what unblocks that), a vcluster-kind target fails with ErrVClusterNotConfigured, not a nil
// dereference or a hang reaching for a credential that is not there.
func TestVClusterTargetFailsClearlyWithoutAConfiguredFactory(t *testing.T) {
	t.Parallel()
	svc := &devloop.OperationsService{}
	_, _, err := svc.PodsFor(context.Background(), vclusterTarget())
	if !errors.Is(err, devloop.ErrVClusterNotConfigured) {
		t.Fatalf("PodsFor(vcluster) with no VClusters = %v, want ErrVClusterNotConfigured", err)
	}
}

// TestVClusterPodsResolvesTheInnerNamespace proves the vcluster kind end to end against a fake
// Secret read: given the vcluster's own kubeconfig, it builds a client scoped to
// VClusterNamespace, inside the vcluster's own API server, not the host namespace.
func TestVClusterPodsResolvesTheInnerNamespace(t *testing.T) {
	t.Parallel()
	target := vclusterTarget()
	reader := &fakeSecretReader{data: map[string][]byte{
		"kagent-staging/vc-kv/config": []byte(minimalKubeconfig),
	}}
	svc := &devloop.OperationsService{VClusters: &devloop.VClusterPods{Secrets: reader}}
	pods, namespace, err := svc.PodsFor(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if pods == nil {
		t.Fatal("PodsFor(vcluster) returned a nil client")
	}
	if namespace != "kagent-dev" {
		t.Fatalf("namespace = %q, want the inner namespace kagent-dev, not the host namespace", namespace)
	}
}

// TestVClusterPodsWrapsASecretReadFailure proves a Secret the reader cannot read (missing RBAC, or
// the Secret not existing yet) is reported as ErrVClusterNotConfigured too: the same clear failure
// as no factory at all, not a raw client-go error.
func TestVClusterPodsWrapsASecretReadFailure(t *testing.T) {
	t.Parallel()
	svc := &devloop.OperationsService{VClusters: &devloop.VClusterPods{Secrets: &fakeSecretReader{err: errors.New("forbidden")}}}
	_, _, err := svc.PodsFor(context.Background(), vclusterTarget())
	if !errors.Is(err, devloop.ErrVClusterNotConfigured) {
		t.Fatalf("PodsFor(vcluster) with a failing secret read = %v, want it to wrap ErrVClusterNotConfigured", err)
	}
}

// TestSyncRunsAgainstAVClusterTarget proves the target-kind abstraction actually drives dev_sync:
// the same sync mechanics (copy, migrate, health, smoke) run unchanged against a vcluster-kind
// target, dispatched through VClusters instead of Pods.
func TestSyncRunsAgainstAVClusterTarget(t *testing.T) {
	t.Parallel()
	d := testdb.New(t)
	seedProject(t, d, "p1", "o/n")
	pods := devlooptest.New()
	pods.BySelector["app=kagent-controller"] = []devloop.PodStatus{{Name: "controller-0", Phase: "Running", Ready: true}}
	migrateCmd, healthCmd, smokeCmd := "go run ./cmd/mainloop migrate up", "curl -fsS http://127.0.0.1:8080/health", "run smoke --read-only now"
	target := vclusterTarget()
	target.MigrateCmd, target.HealthCmd, target.SmokeCmd = &migrateCmd, &healthCmd, &smokeCmd
	if _, err := (&devloop.TargetService{Pool: d.Pool}).Set(context.Background(), "p1", target); err != nil {
		t.Fatal(err)
	}
	sha, pack := scratchRepo(t)
	seed := &devlooptest.Seeder{Payload: pack}
	svc := &devloop.OperationsService{
		Pool: d.Pool, Targets: &devloop.TargetService{Pool: d.Pool}, Seed: seed,
		// VClusters answers every target the same fake pods client devlooptest.Pods already
		// provides for the namespace kind: this test is about the kind dispatch, not about really
		// reading a vcluster's kubeconfig (covered above).
		VClusters:           fakeVClusterFactory{pods: pods, namespace: "kagent-dev"},
		HealthTimeout:       5 * time.Second,
		SettleDelay:         time.Millisecond,
		UncertainRetryDelay: 20 * time.Millisecond,
	}
	ctx := context.Background()
	lease := &devloop.LeaseService{Pool: d.Pool}
	targetRow, err := (&devloop.TargetService{Pool: d.Pool}).Get(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lease.Acquire(ctx, targetRow.ID, "session-a"); err != nil {
		t.Fatal(err)
	}
	runOperations(t, svc, d)

	started, err := svc.Start(ctx, dbstate.DevloopSync, "session-a", "p1", "key-vc", devloop.SyncParams{Repo: "o/n", CommitSHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	row, err := svc.Wait(ctx, started.ID, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != dbstate.DevloopSucceeded {
		t.Fatalf("sync against a vcluster target = %s (%v), want succeeded", row.State, row.Detail)
	}
	if n := pods.CopyCalls["controller"]; n != 1 {
		t.Errorf("controller was copied to %d times, want 1", n)
	}
}

type fakeVClusterFactory struct {
	pods      devloop.PodClient
	namespace string
}

func (f fakeVClusterFactory) ForVCluster(context.Context, devloop.Target) (devloop.PodClient, string, error) {
	return f.pods, f.namespace, nil
}
