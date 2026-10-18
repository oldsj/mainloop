package devloop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// Namespace is the one namespace devloop ever reaches.
const Namespace = "mainloop-dev"

// Channel wakes the operations loop when a row is inserted.
const Channel = "mainloop_devloop_operations"

// SyncParams is dev_sync's durable params.
type SyncParams struct {
	Repo      string `json:"repo"`
	CommitSHA string `json:"commit_sha"`
}

// SyncResult is dev_sync's result once the row is terminal. Each check's field is absent until
// that check ran, so a failed sync shows how far it got.
type SyncResult struct {
	MigrateOK *bool        `json:"migrate_ok,omitempty"`
	HealthOK  *bool        `json:"health_ok,omitempty"`
	Smoke     *SmokeResult `json:"smoke,omitempty"`
}

// SmokeResult is the read-only smoke run's verdict: its own summary line when it passed, the tail
// of its output when it did not.
type SmokeResult struct {
	Passed  bool   `json:"passed"`
	Summary string `json:"summary"`
}

// StatusResult is dev_status's result.
type StatusResult struct {
	Pods []PodStatus `json:"pods"`
}

// Table is the claimable devloop_operations states.
func Table() reconcile.Table {
	return reconcile.NewTable("devloop_operations", string(dbstate.DevloopPending), string(dbstate.DevloopUnknown))
}

// OperationsService drives devloop_operations: sync and status. It is the only devloop
// component with a Kubernetes client, run from serve-api's reconcile.Runner; the MCP listener
// only inserts a row (Start) and polls it (Wait), the boundary described in
// docs/specs/devloop.md.
type OperationsService struct {
	Pool *pgxpool.Pool
	Pods PodClient
	Seed GitSeeder
	// HealthTimeout bounds how long a sync waits for mainloop-dev's own /health after copying
	// (default 180s: a stale instance rebuilds all three Go containers under `go run`).
	HealthTimeout time.Duration
	// MigrateTimeout bounds `migrate up` in the backend container (default 150s; it compiles first).
	MigrateTimeout time.Duration
	// SmokeTimeout bounds the read-only smoke run in the backend container (default 90s).
	SmokeTimeout time.Duration
	// SettleDelay is how long a sync waits after copying before it asks for /health, so the
	// pre-copy process (still answering until watchexec restarts it) cannot pass for the new one
	// (default 3s).
	SettleDelay time.Duration
	// UncertainRetryDelay is how long an uncertain copy waits before the loop retries it
	// (default 10s).
	UncertainRetryDelay time.Duration
}

// Loop is the reconcile.Loop that drives devloop_operations.
func (s *OperationsService) Loop() reconcile.Loop {
	// Syncs are serialized across replicas by the sync lock, not by Concurrency; status reads can
	// overlap them.
	return reconcile.Loop{Name: "devloop-operations", Table: Table(), Handle: s.handle, Wake: []string{Channel}}
}

// Register adds the loop to the runner.
func (s *OperationsService) Register(r *reconcile.Runner) { r.Add(s.Loop()) }

// Start writes (or, by idempotencyKey, reuses) one devloop_operations row and wakes the loop.
func (s *OperationsService) Start(
	ctx context.Context, kind dbstate.DevloopOperationKind, sessionID, projectID, idempotencyKey string, params any,
) (dbq.DevloopOperation, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return dbq.DevloopOperation{}, err
	}
	var row dbq.DevloopOperation
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		inserted, err := q.InsertDevloopOperation(ctx, dbq.InsertDevloopOperationParams{
			ID: idempotencyKey, IdempotencyKey: idempotencyKey, Kind: kind, SessionID: sessionID, ProjectID: projectID, Params: raw,
		})
		switch {
		case err == nil:
			row = inserted
			if kind == dbstate.DevloopSync {
				// A new or reset request is the session's newest: its older waiting syncs must not run
				// later and overwrite it. A repeat call that finds an unfinished row (no insert, no
				// reset) is not newer than anything, so it supersedes nothing.
				if _, err := q.SupersedeDevloopSyncs(ctx, dbq.SupersedeDevloopSyncsParams{SessionID: sessionID, ID: row.ID}); err != nil {
					return err
				}
			}
		case errors.Is(err, pgx.ErrNoRows):
			row, err = q.GetDevloopOperation(ctx, idempotencyKey)
			if err != nil {
				return err
			}
		default:
			return err
		}
		return q.NotifyEvent(ctx, dbq.NotifyEventParams{Channel: Channel, Payload: row.ID})
	})
	return row, err
}

// Wait polls the row until it reaches a terminal state or timeout passes, returning the latest
// known row either way: the caller reads row.State to tell a finished result from a bounded
// timeout still in progress.
func (s *OperationsService) Wait(ctx context.Context, id string, timeout time.Duration) (dbq.DevloopOperation, error) {
	q := dbq.New(s.Pool)
	deadline := time.Now().Add(timeout)
	for {
		row, err := q.GetDevloopOperation(ctx, id)
		if err != nil {
			return dbq.DevloopOperation{}, err
		}
		if row.State == dbstate.DevloopSucceeded || row.State == dbstate.DevloopFailed || !time.Now().Before(deadline) {
			return row, nil
		}
		select {
		case <-ctx.Done():
			return row, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (s *OperationsService) handle(ctx context.Context, c reconcile.Claim) (reconcile.Outcome, error) {
	row, err := dbq.New(s.Pool).GetDevloopOperation(ctx, c.ID)
	if err != nil {
		return reconcile.Retry(), err
	}
	var (
		result any
		state  dbstate.DevloopOperationState
		detail string
		retry  time.Duration
	)
	switch row.Kind {
	case dbstate.DevloopSync:
		// One deadline for the whole sync, from the request: waiting for the sync lock and every
		// retried copy count against it.
		deadline := row.CreatedAt.Add(syncTimeout)
		if !time.Now().Before(deadline) {
			return s.finish(ctx, c, dbstate.DevloopFailed, nil, "the sync did not finish within "+syncTimeout.String())
		}
		syncCtx, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()
		var deferred bool
		result, state, retry, detail, deferred = s.runSync(syncCtx, row, deadline)
		if deferred {
			return reconcile.RetryAfter(retry), nil
		}
	case dbstate.DevloopStatus:
		result, state, detail = s.runStatus(ctx)
	default:
		state, detail = dbstate.DevloopFailed, "unrecognized operation kind"
	}
	if state == dbstate.DevloopUnknown {
		if int(row.Interrupted)+1 >= maxSyncAttempts {
			return s.finish(ctx, c, dbstate.DevloopFailed, nil, fmt.Sprintf("gave up after %d interrupted copies; last: %s", maxSyncAttempts, detail))
		}
		return s.retryAfter(ctx, c, retry, detail)
	}
	return s.finish(ctx, c, state, result, detail)
}

func (s *OperationsService) runStatus(ctx context.Context) (any, dbstate.DevloopOperationState, string) {
	if s.Pods == nil {
		return nil, dbstate.DevloopFailed, "kubernetes is not configured"
	}
	pods, err := s.Pods.ListPods(ctx, Namespace, "")
	if err != nil {
		return nil, dbstate.DevloopFailed, err.Error()
	}
	return StatusResult{Pods: pods}, dbstate.DevloopSucceeded, ""
}

// maxSyncAttempts bounds how many interrupted copies a sync tolerates before it fails, and
// syncTimeout bounds the whole sync from the request, retries and waiting for the sync lock
// included.
const (
	maxSyncAttempts = 5
	syncTimeout     = 10 * time.Minute
	syncLockID      = "mainloop-dev"
	// syncLockWait is how long a sync that found the sync lock taken waits before it asks again.
	syncLockWait = 2 * time.Second
	// syncLockSlack keeps the lock past the holder's deadline so it never lapses mid-copy.
	syncLockSlack = 30 * time.Second
)

// syncTarget is one container devspace.yaml already syncs into.
type syncTarget struct {
	prefix, selector, container, dest string
	exclude                           []string
}

var (
	serverExcludes = []string{".git/", "bin/", "tmp/", "testdata/", "*.test", "*_test.go"}
	webExcludes    = []string{"node_modules/", "dist/", "e2e/", "playwright-report/", "test-results/"}
	syncTargets    = []syncTarget{
		{prefix: "server", selector: "app=mainloop-backend", container: "backend", dest: "/src", exclude: serverExcludes},
		{prefix: "server", selector: "app=mainloop-backend", container: "mcp", dest: "/src", exclude: serverExcludes},
		{prefix: "server", selector: "app=mainloop-backend", container: "git", dest: "/src", exclude: serverExcludes},
		{prefix: "web", selector: frontendSelector, container: "frontend", dest: "/app/web", exclude: webExcludes},
	}
)

// runSync returns deferred true, with the delay in retry, when another sync holds the sync lock.
func (s *OperationsService) runSync(
	ctx context.Context, row dbq.DevloopOperation, deadline time.Time,
) (result any, state dbstate.DevloopOperationState, retry time.Duration, detail string, deferred bool) {
	fail := func(msg string) (any, dbstate.DevloopOperationState, time.Duration, string, bool) {
		return nil, dbstate.DevloopFailed, 0, msg, false
	}
	var p SyncParams
	if err := json.Unmarshal(row.Params, &p); err != nil {
		return fail("invalid params")
	}
	if s.Pods == nil {
		return fail("kubernetes is not configured")
	}
	if s.Seed == nil {
		return fail("github is not configured")
	}
	if msg, err := s.syncStale(ctx, row); err != nil {
		return fail(err.Error())
	} else if msg != "" {
		return fail(msg)
	}
	// One sync at a time across replicas: two copies into the same pods would leave a mix of both
	// trees, and an older one finishing last would leave the older commit behind a "healthy" newer
	// result. The check repeats once the lock is ours, because waiting for it takes time.
	q := dbq.New(s.Pool)
	opID := row.ID
	hold := time.Until(deadline) + syncLockSlack
	got, err := q.TakeDevloopSyncLock(ctx, dbq.TakeDevloopSyncLockParams{ID: syncLockID, OpID: &opID, HoldSecs: hold.Seconds()})
	if err != nil {
		return fail(err.Error())
	}
	if got == 0 {
		return nil, "", syncLockWait, "", true
	}
	defer func() {
		dropCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = q.DropDevloopSyncLock(dropCtx, dbq.DropDevloopSyncLockParams{ID: syncLockID, OpID: &opID})
	}()
	if msg, err := s.syncStale(ctx, row); err != nil {
		return fail(err.Error())
	} else if msg != "" {
		return fail(msg)
	}
	owner, name, ok := strings.Cut(p.Repo, "/")
	if !ok || owner == "" || name == "" {
		return fail("invalid repository")
	}
	tree, err := FetchArchive(ctx, s.Seed, github.Repo{Owner: owner, Name: name}, p.CommitSHA, []string{"server", "web"})
	if err != nil {
		return fail(fmt.Sprintf("fetch %s: %v", p.CommitSHA, err))
	}
	// The pods this sync targets, with their restart counts before the first copy. A container that
	// restarts (or a pod that is replaced) after the copy starts again from the image's source tree,
	// so the tree the sync wrote is gone; every later check compares against this.
	base, err := s.baseline(ctx)
	if err != nil {
		return fail(err.Error())
	}
	for _, t := range syncTargets {
		filtered, err := MirrorTar(bytes.NewReader(tree), t.prefix, t.exclude)
		if err != nil {
			return fail(fmt.Sprintf("filter %s: %v", t.prefix, err))
		}
		pod := base.bySelector(t.selector).pod.Name
		if err := CopyTree(ctx, s.Pods, Namespace, pod, t.container, t.dest, t.exclude, filtered); err != nil {
			switch {
			case ctx.Err() != nil:
				return fail(fmt.Sprintf("the sync did not finish within %s (copy to %s: %v)", syncTimeout, t.container, err))
			case errors.Is(err, ErrCopyInterrupted):
				delay := s.UncertainRetryDelay
				if delay <= 0 {
					delay = 10 * time.Second
				}
				return nil, dbstate.DevloopUnknown, delay,
					fmt.Sprintf("copy to %s was interrupted and will be retried: %v", t.container, err), false
			default:
				return fail(err.Error())
			}
		}
	}
	// The verdict: the schema is migrated, every server listener answers /health from the synced
	// tree, and the read-only smoke run passes. Any miss fails the sync, with the output that
	// explains it.
	var res SyncResult
	backend := base.bySelector(backendSelector).pod.Name
	if err := s.writeMarker(ctx, backend, p.CommitSHA); err != nil {
		return res, dbstate.DevloopFailed, 0, "could not record the synced commit in the backend: " + err.Error(), false
	}
	migrated, detail := s.migrate(ctx, backend)
	res.MigrateOK = &migrated
	if !migrated {
		return res, dbstate.DevloopFailed, 0, detail, false
	}
	healthy, detail := s.waitHealthy(ctx, base, p.CommitSHA)
	res.HealthOK = &healthy
	if !healthy {
		return res, dbstate.DevloopFailed, 0, detail, false
	}
	res.Smoke = s.smoke(ctx, backend)
	if !res.Smoke.Passed {
		return res, dbstate.DevloopFailed, 0, "the read-only smoke run failed:\n" + res.Smoke.Summary, false
	}
	// Once more after the smoke: it compiles and runs from /src too, so a restart in between would
	// have made it pass on the image's tree.
	if msg := s.unstable(ctx, base); msg != "" {
		res.Smoke.Passed = false
		return res, dbstate.DevloopFailed, 0, msg, false
	}
	return res, dbstate.DevloopSucceeded, 0, "", false
}

// The backend container's source tree, where its own `go run` compiles from, and the listeners it
// shares with the mcp and git containers in the same pod. Smoke runs against these loopback ports
// because mainloop-dev's NetworkPolicy admits the Services only from its own frontend and the gateway.
const (
	backendSrc = "/src"
	smokeArgs  = "smoke --read-only --base-url http://127.0.0.1:8000 --mcp-url http://127.0.0.1:8002" +
		" --git-url http://127.0.0.1:8003 --git-push-url http://127.0.0.1:8004"
)

// maxTailLines and maxTailBytes bound the command output a sync attaches to its detail. A crashed
// container's previous log gets more room (maxCrashLines, maxCrashBytes): a Go panic header sits
// above its goroutine dump.
const (
	maxTailLines  = 25
	maxTailBytes  = 3000
	maxCrashLines = 60
	maxCrashBytes = 6000
)

// migrate runs `migrate up` in the backend container from the copied tree. It is idempotent and
// takes the migrate lock, so a concurrent start is safe; the shared dev database is disposable.
func (s *OperationsService) migrate(ctx context.Context, pod string) (bool, string) {
	out, err := s.runMainloop(ctx, pod, orDefault(s.MigrateTimeout, 150*time.Second), "migrate up")
	if err != nil {
		detail := "migrate up failed: " + err.Error() + tailSuffix(out)
		if strings.Contains(out, "missing (out-of-order)") {
			detail += "\n" + outOfOrderRecovery
		}
		return false, detail
	}
	return true, ""
}

// outOfOrderRecovery is what to do when goose refuses because the shared dev database has a
// migration version applied that this tree does not have in order, usually another branch's. The
// sync does not reset the database itself: that wipes mainloop-dev's data, which is the owner's call.
const outOfOrderRecovery = "mainloop-dev's database holds migrations this tree does not have in order " +
	"(usually another branch's). dev_sync never resets it. The owner can run `mainloop migrate reset` in the " +
	"backend container with MAINLOOP_ALLOW_RESET=1, which wipes mainloop-dev's data, then sync again. " +
	"The backend's source tree mirrors the synced commit, so the reset applies only the migrations that commit has."

// smoke runs the existing read-only smoke against the pod's own listeners. It sends no chat turns,
// writes nothing and spends no quota, and it only ever targets 127.0.0.1 inside mainloop-dev.
func (s *OperationsService) smoke(ctx context.Context, pod string) *SmokeResult {
	out, err := s.runMainloop(ctx, pod, orDefault(s.SmokeTimeout, 90*time.Second), smokeArgs)
	if err != nil {
		return &SmokeResult{Passed: false, Summary: err.Error() + tailSuffix(out)}
	}
	summary := "smoke passed"
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "summary:") {
			summary = strings.TrimSpace(line)
		}
	}
	return &SmokeResult{Passed: true, Summary: summary}
}

// runMainloop runs `go run ./cmd/mainloop <args>` in the backend container of pod, which inherits
// that container's environment (database credentials, config). It returns the combined output and,
// when the command did not exit 0, an error saying how it ended.
func (s *OperationsService) runMainloop(ctx context.Context, pod string, timeout time.Duration, args string) (string, error) {
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdout, stderr, err := s.Pods.Exec(stepCtx, Namespace, pod, "backend",
		[]string{"sh", "-c", "cd " + backendSrc + " && exec go run ./cmd/mainloop " + args}, nil)
	out := strings.TrimSpace(stdout + "\n" + stderr)
	if err == nil {
		return out, nil
	}
	// The sync's own context first: its deadline or a lost claim also cancels the step's context, and
	// blaming the step's timeout for that would send the reader to the wrong cap.
	if ctx.Err() != nil {
		return out, fmt.Errorf("the sync was cut off (its deadline is %s from the request, or its claim was lost)", syncTimeout)
	}
	if stepCtx.Err() != nil {
		return out, fmt.Errorf("did not finish within %s", timeout)
	}
	return out, err
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// tailSuffix formats the last lines of a command's output for a detail line.
func tailSuffix(out string) string {
	tail := tailOf(out, maxTailLines, maxTailBytes)
	if tail == "" {
		return ""
	}
	return "\n" + tail
}

// tailOf is the last maxLines lines of out, cut to the last maxBytes bytes on a character boundary
// (the detail column rejects invalid UTF-8); empty when out is blank.
func tailOf(out string, maxLines, maxBytes int) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	tail := strings.Join(lines, "\n")
	if len(tail) > maxBytes {
		tail = strings.ToValidUTF8(tail[len(tail)-maxBytes:], "")
	}
	return strings.TrimSpace(tail)
}

// syncStale reports why a sync must not run now: its session no longer holds the lease (the new
// holder's code would be overwritten), or the session asked for a newer sync since (this one
// would overwrite the newer commit). An empty message means it may run.
func (s *OperationsService) syncStale(ctx context.Context, row dbq.DevloopOperation) (string, error) {
	held, err := (&LeaseService{Pool: s.Pool}).Holds(ctx, row.SessionID)
	if err != nil {
		return "", err
	}
	if !held {
		return "the mainloop-dev lease is no longer held by this session; acquire it again", nil
	}
	newer, err := dbq.New(s.Pool).NewerDevloopSyncExists(ctx, dbq.NewerDevloopSyncExistsParams{
		SessionID: row.SessionID, ID: row.ID, CreatedAt: row.CreatedAt,
	})
	if err != nil {
		return "", err
	}
	if newer {
		return "superseded by a newer sync from this session", nil
	}
	return "", nil
}

// markerFile records, inside the backend container, the commit the sync copied. Nothing but a sync
// writes it, and a restart that puts back the image's tree loses it, so finding it with this
// sync's commit proves the tree under test is the one the container still has.
const markerFile = backendSrc + "/.devloop-commit"

// markerMissExit is the exit status healthScript uses when the marker does not hold the commit.
const markerMissExit = 42

// healthScript asks the three server listeners in the backend pod for /health, after checking the
// marker. It runs inside the pod because mainloop-dev's NetworkPolicy admits port 8000 only from
// its frontend and the gateway, so the production control plane cannot reach the Services over the
// network. The mcp and git listeners rebuild on their own; the smoke needs them up. $1 is the commit.
var healthScript = `[ "$(cat ` + markerFile + ` 2>/dev/null)" = "$1" ] || { echo "marker missing or not $1" >&2; exit ` + fmt.Sprint(markerMissExit) + ` ; }` +
	` ; curl -fsS --max-time 3 -o /dev/null http://127.0.0.1:8000/health` +
	` && curl -fsS --max-time 3 -o /dev/null http://127.0.0.1:8002/health` +
	` && curl -fsS --max-time 3 -o /dev/null http://127.0.0.1:8003/health`

// writeMarker records sha in the backend container's marker file. The sha is an argument, never
// part of the script.
func (s *OperationsService) writeMarker(ctx context.Context, pod, sha string) error {
	_, stderr, err := s.Pods.Exec(ctx, Namespace, pod, "backend",
		[]string{"sh", "-c", `printf %s "$1" > ` + markerFile, "sh", sha}, nil)
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr))
	}
	return nil
}

func (s *OperationsService) waitHealthy(ctx context.Context, base podBaselines, sha string) (bool, string) {
	timeout := orDefault(s.HealthTimeout, 180*time.Second)
	settle := orDefault(s.SettleDelay, 3*time.Second)
	if !sleep(ctx, settle) {
		return false, ctx.Err().Error()
	}
	deadline := time.Now().Add(timeout)
	pod := base.bySelector(backendSelector).pod.Name
	for {
		// A restart or replacement is a failure, not something to wait out: the new container runs
		// the image's tree, and would answer /health for code that is not the commit under test.
		if msg := s.unstable(ctx, base); msg != "" {
			return false, msg
		}
		_, _, err := s.Pods.Exec(ctx, Namespace, pod, "backend",
			[]string{"sh", "-c", healthScript, "sh", sha}, nil)
		if err == nil {
			return true, ""
		}
		var exit exitStatuser
		if errors.As(err, &exit) && exit.ExitStatus() == markerMissExit {
			return false, "the backend's source tree is not the synced commit " + sha +
				" (its marker file is missing or differs): the container was restarted from the image's tree"
		}
		if !time.Now().Before(deadline) {
			detail := "the backend did not report healthy on :8000, :8002 and :8003 within " + timeout.String()
			if lines, err := s.Pods.Logs(ctx, Namespace, pod, "backend", LogOptions{TailLines: 20}); err == nil {
				detail += "; last log lines:\n" + lines
			}
			return false, detail
		}
		if !sleep(ctx, time.Second) {
			return false, ctx.Err().Error()
		}
	}
}

// podBaseline is one target pod as it was before the first copy.
type podBaseline struct {
	label, selector string
	pod             PodStatus
}

type podBaselines []podBaseline

func (b podBaselines) bySelector(selector string) podBaseline {
	for _, p := range b {
		if p.selector == selector {
			return p
		}
	}
	return podBaseline{}
}

// baselineTargets are the pods a sync writes into, by what to call them.
var baselineTargets = []struct{ label, selector string }{
	{"backend", backendSelector},
	{"frontend", frontendSelector},
}

// baseline records the name and restart count of each target pod before the first copy.
func (s *OperationsService) baseline(ctx context.Context) (podBaselines, error) {
	var out podBaselines
	for _, t := range baselineTargets {
		pod, err := s.runningPod(ctx, t.selector)
		if err != nil {
			return nil, err
		}
		out = append(out, podBaseline{label: t.label, selector: t.selector, pod: pod})
	}
	return out, nil
}

// unstable returns why the sync's tree can no longer be trusted, or "" while every target pod is
// still the pod it copied into with the restart count it started with. Containers whose own restart
// count rose are read for their previous log, where the crash shows; one that crashed in an earlier
// sync is not.
func (s *OperationsService) unstable(ctx context.Context, base podBaselines) string {
	for _, b := range base {
		pods, err := s.Pods.ListPods(ctx, Namespace, b.selector)
		if err != nil {
			return fmt.Sprintf("could not re-check mainloop-dev's %s pod: %v", b.label, err)
		}
		var now *PodStatus
		for i := range pods {
			if pods[i].Name == b.pod.Name {
				now = &pods[i]
			}
		}
		switch {
		case now == nil || now.Deleting:
			return fmt.Sprintf("mainloop-dev's %s pod %s was replaced during the sync, which discards the synced tree; "+
				"the commit under test is not what is running.", b.label, b.pod.Name)
		case now.RestartCount > b.pod.RestartCount:
			msg := fmt.Sprintf("mainloop-dev's %s restarted during the sync (restart count %d, was %d), which discards the "+
				"synced tree; the commit under test is not what is running.", b.label, now.RestartCount, b.pod.RestartCount)
			for _, c := range now.Containers {
				if now.ContainerRestarts[c] <= b.pod.ContainerRestarts[c] {
					continue
				}
				if lines, err := s.Pods.Logs(ctx, Namespace, b.pod.Name, c, LogOptions{TailLines: maxCrashLines, Previous: true}); err == nil {
					if tail := tailOf(lines, maxCrashLines, maxCrashBytes); tail != "" {
						msg += fmt.Sprintf("\nprevious %s container, last log lines:\n%s", c, tail)
					}
				}
			}
			return msg
		}
	}
	return ""
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

const (
	backendSelector  = "app=mainloop-backend"
	frontendSelector = "app=mainloop-frontend"
)

// runningPod picks the pod to copy into or exec in: Running and not being deleted, Ready ones
// first. A terminating, evicted or still-starting pod that matches the label is not a target.
func (s *OperationsService) runningPod(ctx context.Context, selector string) (PodStatus, error) {
	pods, err := s.Pods.ListPods(ctx, Namespace, selector)
	if err != nil {
		return PodStatus{}, err
	}
	var pick *PodStatus
	for i := range pods {
		p := &pods[i]
		if p.Phase != "Running" || p.Deleting {
			continue
		}
		if p.Ready {
			return *p, nil
		}
		if pick == nil {
			pick = p
		}
	}
	if pick == nil {
		return PodStatus{}, fmt.Errorf("devloop: no running pod matches %q in %s (%d matched, none Running and not deleting)", selector, Namespace, len(pods))
	}
	return *pick, nil
}

func (s *OperationsService) finish(ctx context.Context, c reconcile.Claim, state dbstate.DevloopOperationState, result any, detail string) (reconcile.Outcome, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		raw = json.RawMessage("null")
	}
	var detailPtr *string
	if detail != "" {
		detailPtr = &detail
	}
	owner := c.Owner
	n, err := dbq.New(s.Pool).FinishDevloopOperation(ctx, dbq.FinishDevloopOperationParams{
		ID: c.ID, LeaseOwner: &owner, Attempts: int32(c.Attempts), State: state, Result: raw, Detail: detailPtr, //nolint:gosec // attempts is a small bounded counter
	})
	if err != nil {
		return reconcile.Outcome{}, err
	}
	if n == 0 {
		return reconcile.Outcome{}, reconcile.ErrLeaseLost
	}
	return reconcile.Recorded(), nil
}

func (s *OperationsService) retryAfter(ctx context.Context, c reconcile.Claim, d time.Duration, detail string) (reconcile.Outcome, error) {
	var detailPtr *string
	if detail != "" {
		detailPtr = &detail
	}
	owner := c.Owner
	n, err := dbq.New(s.Pool).RetryDevloopOperationAfter(ctx, dbq.RetryDevloopOperationAfterParams{
		ID: c.ID, LeaseOwner: &owner, Attempts: int32(c.Attempts), Column4: d.Seconds(), Detail: detailPtr, //nolint:gosec // attempts is a small bounded counter
	})
	if err != nil {
		return reconcile.Outcome{}, err
	}
	if n == 0 {
		return reconcile.Outcome{}, reconcile.ErrLeaseLost
	}
	return reconcile.Recorded(), nil
}
