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

// Channel wakes the operations loop when a row is inserted.
const Channel = "mainloop_devloop_operations"

// SyncParams is dev_sync's durable params.
type SyncParams struct {
	Repo      string `json:"repo"`
	CommitSHA string `json:"commit_sha"`
}

// SyncResult is dev_sync's result once the row is terminal. Each check's field is absent until
// that check ran (the target did not configure it, or the sync did not get that far), so a failed
// sync shows how far it got.
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

// OperationsService drives devloop_operations: sync and status, for whichever project's target the
// row names. It is the only devloop component with a Kubernetes client, run from serve-api's
// reconcile.Runner; the MCP listener only inserts a row (Start) and polls it (Wait), the boundary
// described in docs/specs/devloop.md.
type OperationsService struct {
	Pool *pgxpool.Pool
	// Targets resolves a project's staging target. Required for both Sync and Status; nil answers
	// every operation "no staging target is configured for this project" (never reached in
	// practice, since serve-mcp's own check already refuses a project with none).
	Targets *TargetService
	// Pods is the in-cluster Kubernetes client, used directly for a [TargetNamespace] target.
	Pods PodClient
	// VClusters resolves the client for a [TargetVCluster] target. Nil (the production default
	// until the infra grant in docs/specs/devloop.md exists) fails every such target clearly with
	// [ErrVClusterNotConfigured].
	VClusters VClusterClientFactory
	Seed      GitSeeder
	// HealthTimeout bounds how long a sync waits for the target's health command (and its commit
	// marker) to agree after copying (default 180s: a stale instance can need to rebuild).
	HealthTimeout time.Duration
	// MigrateTimeout, RestartTimeout and SmokeTimeout bound their respective commands in the
	// target's command pod (defaults 150s, 60s, 90s).
	MigrateTimeout time.Duration
	RestartTimeout time.Duration
	SmokeTimeout   time.Duration
	// SettleDelay is how long a sync waits after copying before it checks health, so the pre-copy
	// process (which may keep answering until its own watcher restarts it) cannot pass for the new
	// one (default 3s).
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
	if s.Targets == nil {
		return s.finish(ctx, c, dbstate.DevloopFailed, nil, ErrTargetNotFound.Error())
	}
	target, err := s.Targets.Get(ctx, row.ProjectID)
	if errors.Is(err, ErrTargetNotFound) {
		return s.finish(ctx, c, dbstate.DevloopFailed, nil, err.Error())
	}
	if err != nil {
		return reconcile.Retry(), err
	}
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
		result, state, retry, detail, deferred = s.runSync(syncCtx, row, target, deadline)
		if deferred {
			return reconcile.RetryAfter(retry), nil
		}
	case dbstate.DevloopStatus:
		result, state, detail = s.runStatus(ctx, target)
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

func (s *OperationsService) runStatus(ctx context.Context, target Target) (any, dbstate.DevloopOperationState, string) {
	pods, namespace, err := s.PodsFor(ctx, target)
	if err != nil {
		return nil, dbstate.DevloopFailed, err.Error()
	}
	list, err := pods.ListPods(ctx, namespace, "")
	if err != nil {
		return nil, dbstate.DevloopFailed, err.Error()
	}
	return StatusResult{Pods: list}, dbstate.DevloopSucceeded, ""
}

// maxSyncAttempts bounds how many interrupted copies a sync tolerates before it fails, and
// syncTimeout bounds the whole sync from the request, retries and waiting for the sync lock
// included.
const (
	maxSyncAttempts = 5
	syncTimeout     = 10 * time.Minute
	// syncLockWait is how long a sync that found the sync lock taken waits before it asks again.
	syncLockWait = 2 * time.Second
	// syncLockSlack keeps the lock past the holder's deadline so it never lapses mid-copy.
	syncLockSlack = 30 * time.Second
)

// runSync returns deferred true, with the delay in retry, when another sync holds the sync lock.
func (s *OperationsService) runSync(
	ctx context.Context, row dbq.DevloopOperation, target Target, deadline time.Time,
) (result any, state dbstate.DevloopOperationState, retry time.Duration, detail string, deferred bool) {
	fail := func(msg string) (any, dbstate.DevloopOperationState, time.Duration, string, bool) {
		return nil, dbstate.DevloopFailed, 0, msg, false
	}
	var p SyncParams
	if err := json.Unmarshal(row.Params, &p); err != nil {
		return fail("invalid params")
	}
	cmdPath, ok := commandSyncPath(target)
	if !ok {
		return fail(ErrInvalidTarget.Error() + ": workload/command_container must match one sync path")
	}
	pods, namespace, err := s.PodsFor(ctx, target)
	if err != nil {
		return fail(err.Error())
	}
	if s.Seed == nil {
		return fail("github is not configured")
	}
	if msg, err := s.syncStale(ctx, target.ID, row); err != nil {
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
	got, err := q.TakeDevloopSyncLock(ctx, dbq.TakeDevloopSyncLockParams{ID: target.ID, OpID: &opID, HoldSecs: hold.Seconds()})
	if err != nil {
		return fail(err.Error())
	}
	if got == 0 {
		return nil, "", syncLockWait, "", true
	}
	defer func() {
		dropCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = q.DropDevloopSyncLock(dropCtx, dbq.DropDevloopSyncLockParams{ID: target.ID, OpID: &opID})
	}()
	if msg, err := s.syncStale(ctx, target.ID, row); err != nil {
		return fail(err.Error())
	} else if msg != "" {
		return fail(msg)
	}
	owner, name, ok := strings.Cut(p.Repo, "/")
	if !ok || owner == "" || name == "" {
		return fail("invalid repository")
	}
	tree, err := FetchArchive(ctx, s.Seed, github.Repo{Owner: owner, Name: name}, p.CommitSHA, syncPrefixes(target.SyncPaths))
	if err != nil {
		return fail(fmt.Sprintf("fetch %s: %v", p.CommitSHA, err))
	}
	run := &syncRun{OperationsService: s, pods: pods, namespace: namespace, target: target}
	// The pods this sync targets, with their restart counts before the first copy. A container that
	// restarts (or a pod that is replaced) after the copy starts again from the image's source tree,
	// so the tree the sync wrote is gone; every later check compares against this.
	base, err := run.baseline(ctx)
	if err != nil {
		return fail(err.Error())
	}
	for _, t := range target.SyncPaths {
		filtered, err := MirrorTar(bytes.NewReader(tree), t.Prefix, t.Exclude)
		if err != nil {
			return fail(fmt.Sprintf("filter %s: %v", t.Prefix, err))
		}
		pod := base.bySelector(t.Selector).pod.Name
		if err := CopyTree(ctx, pods, namespace, pod, t.Container, t.Dest, t.Exclude, filtered); err != nil {
			switch {
			case ctx.Err() != nil:
				return fail(fmt.Sprintf("the sync did not finish within %s (copy to %s: %v)", syncTimeout, t.Container, err))
			case errors.Is(err, ErrCopyInterrupted):
				delay := s.UncertainRetryDelay
				if delay <= 0 {
					delay = 10 * time.Second
				}
				return nil, dbstate.DevloopUnknown, delay,
					fmt.Sprintf("copy to %s was interrupted and will be retried: %v", t.Container, err), false
			default:
				return fail(err.Error())
			}
		}
	}
	// The verdict: the schema is migrated, the target's health command (if any) agrees the synced
	// tree is live, and the read-only smoke run passes. Any miss fails the sync, with the output
	// that explains it; a command the target did not configure is skipped, not failed.
	var res SyncResult
	cmdPod := base.bySelector(target.Workload).pod.Name
	markerFile := cmdPath.Dest + "/.devloop-commit"
	if err := run.writeMarker(ctx, cmdPod, markerFile, p.CommitSHA); err != nil {
		return res, dbstate.DevloopFailed, 0, "could not record the synced commit: " + err.Error(), false
	}
	if target.RestartCmd != nil {
		if _, detail := run.exec(ctx, cmdPod, orDefault(s.RestartTimeout, 60*time.Second), cmdPath.Dest, *target.RestartCmd); detail != "" {
			return res, dbstate.DevloopFailed, 0, "restart failed: " + detail, false
		}
	}
	if target.MigrateCmd != nil {
		migrated, detail := run.migrate(ctx, cmdPod, cmdPath.Dest, *target.MigrateCmd)
		res.MigrateOK = &migrated
		if !migrated {
			return res, dbstate.DevloopFailed, 0, detail, false
		}
	}
	healthy, detail := run.waitHealthy(ctx, base, cmdPath.Dest, markerFile, p.CommitSHA)
	res.HealthOK = &healthy
	if !healthy {
		return res, dbstate.DevloopFailed, 0, detail, false
	}
	if target.SmokeCmd != nil {
		res.Smoke = run.smoke(ctx, cmdPod, cmdPath.Dest)
		if !res.Smoke.Passed {
			return res, dbstate.DevloopFailed, 0, "the read-only smoke run failed:\n" + res.Smoke.Summary, false
		}
	}
	// Once more after the smoke: the command pod compiles and runs from the synced tree too, so a
	// restart in between would have made it pass on the image's tree.
	if msg := run.unstable(ctx, base); msg != "" {
		if res.Smoke != nil {
			res.Smoke.Passed = false
		}
		return res, dbstate.DevloopFailed, 0, msg, false
	}
	return res, dbstate.DevloopSucceeded, 0, "", false
}

// syncPrefixes is the distinct repository subtrees target.SyncPaths copies from, for FetchArchive.
func syncPrefixes(paths []SyncPath) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		if !seen[p.Prefix] {
			seen[p.Prefix] = true
			out = append(out, p.Prefix)
		}
	}
	return out
}

// maxTailLines and maxTailBytes bound the command output a sync attaches to its detail. A crashed
// container's previous log gets more room (maxCrashLines, maxCrashBytes): a Go panic header sits
// above its goroutine dump.
const (
	maxTailLines  = 25
	maxTailBytes  = 3000
	maxCrashLines = 60
	maxCrashBytes = 6000
)

// outOfOrderRecovery is what to do when goose refuses because the shared dev database has a
// migration version applied that this tree does not have in order, usually another branch's. The
// sync does not reset the database itself: that wipes the target's data, which is the owner's call.
// This recovery text is specific to Mainloop's own goose-based migrate command (as is detecting
// "missing (out-of-order)" below): a target configured with a different migration tool only gets
// the command's own output, without this added guidance.
const outOfOrderRecovery = "mainloop-dev's database holds migrations this tree does not have in order " +
	"(usually another branch's). dev_sync never resets it. The owner can run `mainloop migrate reset` in the " +
	"backend container with MAINLOOP_ALLOW_RESET=1, which wipes mainloop-dev's data, then sync again. " +
	"The backend's source tree mirrors the synced commit, so the reset applies only the migrations that commit has."

// syncRun is one sync's view of its target: the resolved Kubernetes client and namespace, and the
// target config, bundled so the per-step helpers below do not each take every one of those as a
// parameter. Embedding *OperationsService gives them its Pool and configured timeouts.
type syncRun struct {
	*OperationsService
	pods      PodClient
	namespace string
	target    Target
}

// migrate runs target's migrate command in the command pod, from workdir. It is expected to be
// idempotent (the target's own migration tool should take its own lock); the dev database it runs
// against is disposable.
func (r *syncRun) migrate(ctx context.Context, pod, workdir, cmd string) (bool, string) {
	out, detail := r.exec(ctx, pod, orDefault(r.MigrateTimeout, 150*time.Second), workdir, cmd)
	if detail == "" {
		return true, ""
	}
	full := "migrate up failed: " + detail
	if strings.Contains(out, "missing (out-of-order)") {
		full += "\n" + outOfOrderRecovery
	}
	return false, full
}

// smoke runs target's smoke command in the command pod. It is expected to be read-only: devloop
// sends no chat turn, writes nothing and spends no quota of its own, but what the command itself
// does is the target's responsibility.
func (r *syncRun) smoke(ctx context.Context, pod, workdir string) *SmokeResult {
	out, detail := r.exec(ctx, pod, orDefault(r.SmokeTimeout, 90*time.Second), workdir, *r.target.SmokeCmd)
	if detail != "" {
		return &SmokeResult{Passed: false, Summary: detail}
	}
	summary := "smoke passed"
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "summary:") {
			summary = strings.TrimSpace(line)
		}
	}
	return &SmokeResult{Passed: true, Summary: summary}
}

// exec runs `cd workdir && exec cmd` in pod's command container, and returns the combined output
// and, when the command did not exit 0, a detail line (with its output tail) explaining how it
// ended; empty when it succeeded.
func (r *syncRun) exec(ctx context.Context, pod string, timeout time.Duration, workdir, cmd string) (string, string) {
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	script := cmd
	if workdir != "" {
		script = "cd " + workdir + " && exec " + cmd
	}
	stdout, stderr, err := r.pods.Exec(stepCtx, r.namespace, pod, r.target.CommandContainer,
		[]string{"sh", "-c", script}, nil)
	out := strings.TrimSpace(stdout + "\n" + stderr)
	if err == nil {
		return out, ""
	}
	// The sync's own context first: its deadline or a lost claim also cancels the step's context, and
	// blaming the step's timeout for that would send the reader to the wrong cap.
	switch {
	case ctx.Err() != nil:
		return out, fmt.Sprintf("the sync was cut off (its deadline is %s from the request, or its claim was lost)", syncTimeout)
	case stepCtx.Err() != nil:
		return out, fmt.Sprintf("did not finish within %s%s", timeout, tailSuffix(out))
	default:
		return out, err.Error() + tailSuffix(out)
	}
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

// syncStale reports why a sync must not run now: its session no longer holds leaseID (the new
// holder's code would be overwritten), or the session asked for a newer sync since (this one
// would overwrite the newer commit). An empty message means it may run.
func (s *OperationsService) syncStale(ctx context.Context, leaseID string, row dbq.DevloopOperation) (string, error) {
	held, err := (&LeaseService{Pool: s.Pool}).Holds(ctx, leaseID, row.SessionID)
	if err != nil {
		return "", err
	}
	if !held {
		return "the lease is no longer held by this session; acquire it again", nil
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

// markerMissExit is the exit status healthScript uses when the marker does not hold the commit.
const markerMissExit = 42

// markerScript asks whether markerFile holds commit, the way every target's health check is
// guarded: the command pod's source tree must still be the commit this sync copied, not an image's
// tree restored by a restart. $1 is the commit.
func markerScript(markerFile string) string {
	return `[ "$(cat ` + markerFile + ` 2>/dev/null)" = "$1" ] || { echo "marker missing or not $1" >&2; exit ` +
		fmt.Sprint(markerMissExit) + ` ; }`
}

// writeMarker records sha in pod's markerFile. The sha is an argument, never part of the script.
func (r *syncRun) writeMarker(ctx context.Context, pod, markerFile, sha string) error {
	_, stderr, err := r.pods.Exec(ctx, r.namespace, pod, r.target.CommandContainer,
		[]string{"sh", "-c", `printf %s "$1" > ` + markerFile, "sh", sha}, nil)
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr))
	}
	return nil
}

// waitHealthy waits for the marker to confirm the commit and, when the target configures one, its
// health command to agree, retrying until HealthTimeout. A target with no health command still
// gets the marker check (devloop's own stability guard), just without a liveness probe.
func (r *syncRun) waitHealthy(ctx context.Context, base podBaselines, workdir, markerFile, sha string) (bool, string) {
	timeout := orDefault(r.HealthTimeout, 180*time.Second)
	settle := orDefault(r.SettleDelay, 3*time.Second)
	if !sleep(ctx, settle) {
		return false, ctx.Err().Error()
	}
	deadline := time.Now().Add(timeout)
	pod := base.bySelector(r.target.Workload).pod.Name
	script := markerScript(markerFile)
	if r.target.HealthCmd != nil {
		script += " ; " + *r.target.HealthCmd
	}
	if workdir != "" {
		script = "cd " + workdir + " && " + script
	}
	for {
		// A restart or replacement is a failure, not something to wait out: the new container runs
		// the image's tree, and would answer healthy for code that is not the commit under test.
		if msg := r.unstable(ctx, base); msg != "" {
			return false, msg
		}
		_, _, err := r.pods.Exec(ctx, r.namespace, pod, r.target.CommandContainer,
			[]string{"sh", "-c", script, "sh", sha}, nil)
		if err == nil {
			return true, ""
		}
		var exit exitStatuser
		if errors.As(err, &exit) && exit.ExitStatus() == markerMissExit {
			return false, "the command pod's source tree is not the synced commit " + sha +
				" (its marker file is missing or differs): the container was restarted from the image's tree"
		}
		if !time.Now().Before(deadline) {
			detail := "the target did not report healthy within " + timeout.String()
			if lines, err := r.pods.Logs(ctx, r.namespace, pod, r.target.CommandContainer, LogOptions{TailLines: 20}); err == nil {
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
	selector string
	pod      PodStatus
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

// distinctSelectors are the selectors a target's sync paths name, in first-seen order: every pod a
// sync writes into, which [syncRun.baseline] and [syncRun.unstable] must watch for a restart or
// replacement after the copy.
func distinctSelectors(paths []SyncPath) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		if !seen[p.Selector] {
			seen[p.Selector] = true
			out = append(out, p.Selector)
		}
	}
	return out
}

// baseline records the name and restart count of each target pod before the first copy.
func (r *syncRun) baseline(ctx context.Context) (podBaselines, error) {
	var out podBaselines
	for _, selector := range distinctSelectors(r.target.SyncPaths) {
		pod, err := r.runningPod(ctx, selector)
		if err != nil {
			return nil, err
		}
		out = append(out, podBaseline{selector: selector, pod: pod})
	}
	return out, nil
}

// unstable returns why the sync's tree can no longer be trusted, or "" while every target pod is
// still the pod it copied into with the restart count it started with. Containers whose own restart
// count rose are read for their previous log, where the crash shows; one that crashed in an earlier
// sync is not.
func (r *syncRun) unstable(ctx context.Context, base podBaselines) string {
	for _, b := range base {
		pods, err := r.pods.ListPods(ctx, r.namespace, b.selector)
		if err != nil {
			return fmt.Sprintf("could not re-check the %s pod: %v", b.selector, err)
		}
		var now *PodStatus
		for i := range pods {
			if pods[i].Name == b.pod.Name {
				now = &pods[i]
			}
		}
		switch {
		case now == nil || now.Deleting:
			return fmt.Sprintf("the %s pod %s was replaced during the sync, which discards the synced tree; "+
				"the commit under test is not what is running.", b.selector, b.pod.Name)
		case now.RestartCount > b.pod.RestartCount:
			msg := fmt.Sprintf("the %s pod restarted during the sync (restart count %d, was %d), which discards the "+
				"synced tree; the commit under test is not what is running.", b.selector, now.RestartCount, b.pod.RestartCount)
			for _, c := range now.Containers {
				if now.ContainerRestarts[c] <= b.pod.ContainerRestarts[c] {
					continue
				}
				if lines, err := r.pods.Logs(ctx, r.namespace, b.pod.Name, c, LogOptions{TailLines: maxCrashLines, Previous: true}); err == nil {
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

// runningPod picks the pod to copy into or exec in: Running and not being deleted, Ready ones
// first. A terminating, evicted or still-starting pod that matches the label is not a target.
func (r *syncRun) runningPod(ctx context.Context, selector string) (PodStatus, error) {
	pods, err := r.pods.ListPods(ctx, r.namespace, selector)
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
		return PodStatus{}, fmt.Errorf("devloop: no running pod matches %q in %s (%d matched, none Running and not deleting)", selector, r.namespace, len(pods))
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
