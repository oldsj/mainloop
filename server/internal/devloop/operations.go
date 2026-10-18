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

// SyncResult is dev_sync's result once the row is terminal.
type SyncResult struct {
	HealthOK bool `json:"health_ok"`
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
	// (default 60s).
	HealthTimeout time.Duration
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
	syncTimeout     = 5 * time.Minute
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
		{prefix: "web", selector: "app=mainloop-frontend", container: "frontend", dest: "/app/web", exclude: webExcludes},
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
	for _, t := range syncTargets {
		filtered, err := FilterTar(bytes.NewReader(tree), t.prefix, t.exclude)
		if err != nil {
			return fail(fmt.Sprintf("filter %s: %v", t.prefix, err))
		}
		pod, err := s.onePodBySelector(ctx, t.selector)
		if err != nil {
			return fail(err.Error())
		}
		if err := CopyTree(ctx, s.Pods, Namespace, pod, t.container, t.dest, filtered); err != nil {
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
	healthy, detail := s.waitHealthy(ctx)
	return SyncResult{HealthOK: healthy}, dbstate.DevloopSucceeded, 0, detail, false
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

// backendHealthCommand asks the backend container's own listener for /health. It runs inside the
// pod because mainloop-dev's NetworkPolicy admits port 8000 only from its frontend and the
// gateway, so the production control plane cannot reach the Service over the network.
var backendHealthCommand = []string{"curl", "-fsS", "--max-time", "3", "http://127.0.0.1:8000/health"}

func (s *OperationsService) waitHealthy(ctx context.Context) (bool, string) {
	timeout := s.HealthTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	settle := s.SettleDelay
	if settle <= 0 {
		settle = 3 * time.Second
	}
	if !sleep(ctx, settle) {
		return false, ctx.Err().Error()
	}
	deadline := time.Now().Add(timeout)
	pod, podErr := s.onePodBySelector(ctx, "app=mainloop-backend")
	for {
		if podErr == nil {
			if _, _, err := s.Pods.Exec(ctx, Namespace, pod, "backend", backendHealthCommand, nil); err == nil {
				return true, ""
			}
		}
		if !time.Now().Before(deadline) {
			detail := "the backend did not report healthy within the timeout"
			if podErr == nil {
				if lines, err := s.Pods.Logs(ctx, Namespace, pod, "backend", LogOptions{TailLines: 20}); err == nil {
					detail += "; last log lines:\n" + lines
				}
			}
			return false, detail
		}
		if !sleep(ctx, time.Second) {
			return false, ctx.Err().Error()
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func (s *OperationsService) onePodBySelector(ctx context.Context, selector string) (string, error) {
	pods, err := s.Pods.ListPods(ctx, Namespace, selector)
	if err != nil {
		return "", err
	}
	if len(pods) == 0 {
		return "", fmt.Errorf("devloop: no pod matches %q in %s", selector, Namespace)
	}
	return pods[0].Name, nil
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
