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

// LogsParams is dev_logs's durable params.
type LogsParams struct {
	Container string `json:"container"`
	TailLines int64  `json:"tail_lines"`
}

// LogsResult is dev_logs's result.
type LogsResult struct {
	Lines string `json:"lines"`
}

// SmokeResult is dev_smoke's result.
type SmokeResult struct {
	Checks []SmokeCheck `json:"checks"`
	Failed bool         `json:"failed"`
}

// Table is the claimable devloop_operations states.
func Table() reconcile.Table {
	return reconcile.NewTable("devloop_operations", string(dbstate.DevloopPending), string(dbstate.DevloopUnknown))
}

// OperationsService drives devloop_operations: sync, status, logs, smoke. It is the only devloop
// component with a Kubernetes client, run from serve-api's reconcile.Runner; the MCP listener
// only inserts a row (Start) and polls it (Wait), the boundary described in
// docs/specs/devloop.md.
type OperationsService struct {
	Pool   *pgxpool.Pool
	Pods   PodClient
	Seed   GitSeeder
	Health HealthChecker
	Smoke  SmokeRunner
	// HealthTimeout bounds how long a sync waits for mainloop-dev's own /health after copying
	// (default 60s).
	HealthTimeout time.Duration
	// UncertainRetryDelay is how long an uncertain copy waits before the loop retries it
	// (default 10s).
	UncertainRetryDelay time.Duration
}

// Loop is the reconcile.Loop that drives devloop_operations.
func (s *OperationsService) Loop() reconcile.Loop {
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
		result, state, retry, detail = s.runSync(ctx, row)
	case dbstate.DevloopStatus:
		result, state, detail = s.runStatus(ctx)
	case dbstate.DevloopLogs:
		result, state, detail = s.runLogs(ctx, row)
	case dbstate.DevloopSmoke:
		result, state, detail = s.runSmoke(ctx)
	default:
		state, detail = dbstate.DevloopFailed, "unrecognized operation kind"
	}
	if state == dbstate.DevloopUnknown {
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

func (s *OperationsService) runLogs(ctx context.Context, row dbq.DevloopOperation) (any, dbstate.DevloopOperationState, string) {
	if s.Pods == nil {
		return nil, dbstate.DevloopFailed, "kubernetes is not configured"
	}
	var p LogsParams
	if err := json.Unmarshal(row.Params, &p); err != nil {
		return nil, dbstate.DevloopFailed, "invalid params"
	}
	pod, err := s.onePod(ctx, p.Container)
	if err != nil {
		return nil, dbstate.DevloopFailed, err.Error()
	}
	lines, err := s.Pods.Logs(ctx, Namespace, pod, p.Container, LogOptions{TailLines: p.TailLines})
	if err != nil {
		return nil, dbstate.DevloopFailed, err.Error()
	}
	return LogsResult{Lines: lines}, dbstate.DevloopSucceeded, ""
}

func (s *OperationsService) runSmoke(ctx context.Context) (any, dbstate.DevloopOperationState, string) {
	if s.Smoke == nil {
		return nil, dbstate.DevloopFailed, "smoke is not configured"
	}
	checks, failed, err := s.Smoke.Run(ctx)
	if err != nil {
		return nil, dbstate.DevloopFailed, err.Error()
	}
	detail := ""
	if failed {
		detail = "one or more checks failed"
	}
	return SmokeResult{Checks: checks, Failed: failed}, dbstate.DevloopSucceeded, detail
}

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

func (s *OperationsService) runSync(ctx context.Context, row dbq.DevloopOperation) (any, dbstate.DevloopOperationState, time.Duration, string) {
	var p SyncParams
	if err := json.Unmarshal(row.Params, &p); err != nil {
		return nil, dbstate.DevloopFailed, 0, "invalid params"
	}
	if s.Pods == nil {
		return nil, dbstate.DevloopFailed, 0, "kubernetes is not configured"
	}
	if s.Seed == nil {
		return nil, dbstate.DevloopFailed, 0, "github is not configured"
	}
	owner, name, ok := strings.Cut(p.Repo, "/")
	if !ok || owner == "" || name == "" {
		return nil, dbstate.DevloopFailed, 0, "invalid repository"
	}
	tree, err := FetchArchive(ctx, s.Seed, github.Repo{Owner: owner, Name: name}, p.CommitSHA, []string{"server", "web"})
	if err != nil {
		return nil, dbstate.DevloopFailed, 0, fmt.Sprintf("fetch %s: %v", p.CommitSHA, err)
	}
	for _, t := range syncTargets {
		filtered, err := FilterTar(bytes.NewReader(tree), t.prefix, t.exclude)
		if err != nil {
			return nil, dbstate.DevloopFailed, 0, fmt.Sprintf("filter %s: %v", t.prefix, err)
		}
		pod, err := s.onePodBySelector(ctx, t.selector)
		if err != nil {
			return nil, dbstate.DevloopFailed, 0, err.Error()
		}
		if err := CopyTree(ctx, s.Pods, Namespace, pod, t.container, t.dest, filtered); err != nil {
			if isUncertainCopyError(err) {
				delay := s.UncertainRetryDelay
				if delay <= 0 {
					delay = 10 * time.Second
				}
				return nil, dbstate.DevloopUnknown, delay,
					fmt.Sprintf("copy to %s was interrupted and will be retried: %v", t.container, err)
			}
			return nil, dbstate.DevloopFailed, 0, err.Error()
		}
	}
	if s.Health == nil {
		return SyncResult{HealthOK: false}, dbstate.DevloopSucceeded, 0, "copied; health is not configured"
	}
	healthy, detail := s.waitHealthy(ctx)
	return SyncResult{HealthOK: healthy}, dbstate.DevloopSucceeded, 0, detail
}

func (s *OperationsService) waitHealthy(ctx context.Context) (bool, string) {
	timeout := s.HealthTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		if ok, err := s.Health.Healthy(ctx); err == nil && ok {
			return true, ""
		}
		if !time.Now().Before(deadline) {
			detail := "the backend did not report healthy within the timeout"
			if pod, err := s.onePodBySelector(ctx, "app=mainloop-backend"); err == nil {
				if lines, err := s.Pods.Logs(ctx, Namespace, pod, "backend", LogOptions{TailLines: 20}); err == nil {
					detail += "; last log lines:\n" + lines
				}
			}
			return false, detail
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err().Error()
		case <-time.After(2 * time.Second):
		}
	}
}

// isUncertainCopyError reports whether a copy's outcome could not be observed: the stream ended
// without telling us whether tar on the other side finished extracting. Unlike a git push, a tar
// overwrite of the same commit's tree is idempotent, so the loop resolves this by retrying the
// copy itself rather than first observing remote state.
func isUncertainCopyError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	for _, s := range []string{"EOF", "closed", "reset", "timeout"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

func (s *OperationsService) onePod(ctx context.Context, container string) (string, error) {
	selector, ok := containerSelector[container]
	if !ok {
		return "", fmt.Errorf("devloop: unknown container %q", container)
	}
	return s.onePodBySelector(ctx, selector)
}

var containerSelector = map[string]string{
	"backend": "app=mainloop-backend", "mcp": "app=mainloop-backend", "git": "app=mainloop-backend",
	"frontend": "app=mainloop-frontend",
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
