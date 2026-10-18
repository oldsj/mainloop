package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/devloop"
)

// devloopProject is the one project whose task agents may use the devloop tools (see
// docs/specs/devloop.md). Visibility is gated by the MAINLOOP_DEVLOOP_ENABLED flag through
// [agentauth.Policy.DevLoop]; this is the further, handler-level check the flag alone cannot
// express.
const devloopProject = "oldsj/mainloop"

// devLoopWaitBound is how long each tool call waits for its devloop_operations row to finish
// before answering with whatever is known so far; the row itself (and serve-api's reconcile loop)
// keeps working past this bound.
const devLoopWaitBound = 4 * time.Minute

// devLoopTools let a task agent of devloopProject iterate on the shared mainloop-dev instance:
// one FIFO lease, a sync that fetches a pushed commit and copies it into mainloop-dev's synced
// containers, and a read-only pod status. The MCP listener never reaches Kubernetes itself;
// every tool but the lease ones writes a devloop_operations row and polls it,
// and serve-api's reconcile loop (the only holder of a Kubernetes client) does the work.
func (s *Server) devLoopTools() []Tool {
	none := objectSchema(map[string]any{})
	sha := map[string]any{"type": "string", "pattern": "^[0-9a-f]{40}$"}
	return []Tool{
		{
			Name: agentauth.ToolDevLeaseAcquire,
			Description: "Acquire the one live-sync lease on mainloop-dev, or report your place in its FIFO " +
				"queue. An idle-expired lease is taken over by the earliest queued session. A queued " +
				"session must call this again within 2 minutes to keep its place.",
			InputSchema: none, Handle: s.devLeaseAcquire,
		},
		{
			Name:        agentauth.ToolDevLeaseRelease,
			Description: "Release the mainloop-dev lease if this session holds it.",
			InputSchema: none, Handle: s.devLeaseRelease,
		},
		{
			Name: agentauth.ToolDevSync,
			Description: "Use after pushing and before open_pull_request. Syncs commit_sha (already pushed through " +
				"the git push transport) into mainloop-dev, migrates its database, waits for its /health, and runs " +
				"the read-only smoke check. Succeeds only when mainloop-dev is healthy and the smoke passes; any " +
				"other result is failed, with the output that explains it. Requires holding the lease. Waits up to " +
				"4 minutes, then reports the state so far; retrying with the same commit_sha is safe.",
			InputSchema: objectSchema(map[string]any{"commit_sha": sha}, "commit_sha"), Handle: s.devSync,
		},
		{
			Name:        agentauth.ToolDevStatus,
			Description: "Read mainloop-dev's pod status: phase, readiness and restart counts.",
			InputSchema: none, Handle: s.devStatus,
		},
	}
}

// devLoopAuthority fails closed unless the actor is bound to devloopProject.
func (s *Server) devLoopAuthority(ctx context.Context, a agentauth.Actor) error {
	if a.ProjectID == "" {
		return failure("forbidden", "this session has no project")
	}
	full, err := s.Queries.GetProjectFullName(ctx, a.ProjectID)
	if err != nil {
		return failure("forbidden", "this session has no project")
	}
	if !strings.EqualFold(full, devloopProject) {
		return failure("forbidden", "devloop tools are limited to "+devloopProject)
	}
	return nil
}

func (s *Server) devLeaseAcquire(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	if err := decode(raw, &struct{}{}); err != nil {
		return Result{}, err
	}
	if err := s.devLoopAuthority(ctx, a); err != nil {
		return Result{}, err
	}
	if s.DevLease == nil {
		return Result{}, failure("unavailable", "devloop is not available")
	}
	info, err := s.DevLease.Acquire(ctx, a.BindingID)
	if err != nil {
		return Result{}, err
	}
	return leaseResult(info), nil
}

func (s *Server) devLeaseRelease(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	if err := decode(raw, &struct{}{}); err != nil {
		return Result{}, err
	}
	if err := s.devLoopAuthority(ctx, a); err != nil {
		return Result{}, err
	}
	if s.DevLease == nil {
		return Result{}, failure("unavailable", "devloop is not available")
	}
	if err := s.DevLease.Release(ctx, a.BindingID); err != nil {
		return Result{}, err
	}
	return Result{Text: "lease released", Structured: map[string]any{"text": "lease released"}}, nil
}

func leaseResult(info devloop.LeaseInfo) Result {
	m := map[string]any{"status": string(info.Status), "idle_timeout_seconds": int(info.IdleTimeout.Seconds())}
	if info.HolderSession != "" {
		m["holder_session"] = info.HolderSession
	}
	if info.AcquiredAt != nil {
		m["acquired_at"] = info.AcquiredAt.UTC().Format(time.RFC3339)
	}
	if info.LastUsedAt != nil {
		m["last_used_at"] = info.LastUsedAt.UTC().Format(time.RFC3339)
	}
	line := "lease " + string(info.Status)
	if info.Status == devloop.LeaseQueued {
		m["position"], m["queue_length"] = info.Position, info.QueueLength
		line = fmt.Sprintf("lease queued at position %d of %d, held by %s; call dev_lease_acquire again within 2 minutes to keep your place", info.Position, info.QueueLength, info.HolderSession)
	}
	m["text"] = line
	return Result{Text: line, Structured: m}
}

func (s *Server) devSync(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	var in struct {
		CommitSHA string `json:"commit_sha"`
	}
	if err := decode(raw, &in); err != nil {
		return Result{}, err
	}
	if !isSHA(in.CommitSHA) {
		return Result{}, errValidation
	}
	if err := s.devLoopAuthority(ctx, a); err != nil {
		return Result{}, err
	}
	if s.DevLease == nil || s.DevOps == nil {
		return Result{}, failure("unavailable", "devloop is not available")
	}
	// Using the lease is also its heartbeat, so a session that syncs steadily never idles out.
	if err := s.DevLease.Use(ctx, a.BindingID); err != nil {
		if errors.Is(err, devloop.ErrNotHolder) {
			return Result{}, failure("lease_required", "acquire the mainloop-dev lease with dev_lease_acquire first")
		}
		return Result{}, err
	}
	full, err := s.Queries.GetProjectFullName(ctx, a.ProjectID)
	if err != nil {
		return Result{}, err
	}
	row, err := s.DevOps.Start(ctx, dbstate.DevloopSync, a.BindingID, a.ProjectID, syncIdempotencyKey(a.BindingID, in.CommitSHA),
		devloop.SyncParams{Repo: full, CommitSHA: in.CommitSHA})
	if err != nil {
		return Result{}, err
	}
	final, err := s.DevOps.Wait(ctx, row.ID, devLoopWaitBound)
	if err != nil {
		return Result{}, err
	}
	return devOpResult(final), nil
}

// syncIdempotencyKey collapses a retried dev_sync call for the same session and commit onto the
// one durable row, the way pushgate's publication id collapses a retried push.
func syncIdempotencyKey(sessionID, commitSHA string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + commitSHA))
	return hex.EncodeToString(sum[:])
}

func (s *Server) devStatus(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	if err := decode(raw, &struct{}{}); err != nil {
		return Result{}, err
	}
	if err := s.devLoopAuthority(ctx, a); err != nil {
		return Result{}, err
	}
	if s.DevOps == nil {
		return Result{}, failure("unavailable", "devloop is not available")
	}
	row, err := s.DevOps.Start(ctx, dbstate.DevloopStatus, a.BindingID, a.ProjectID, uuid.NewString(), struct{}{})
	if err != nil {
		return Result{}, err
	}
	final, err := s.DevOps.Wait(ctx, row.ID, 15*time.Second)
	if err != nil {
		return Result{}, err
	}
	return devOpResult(final), nil
}

// operationResult shows a devloop_operations row's current state: terminal (succeeded/failed) or
// still in flight after devLoopWaitBound, in which case the agent reads state and may call the
// same tool again (a repeated sync joins the row still in flight; status is cheap to repeat).
func devOpResult(row dbq.DevloopOperation) Result {
	m := map[string]any{"id": row.ID, "state": string(row.State)}
	if row.Detail != nil {
		m["detail"] = *row.Detail
	}
	if len(row.Result) > 0 && string(row.Result) != "null" {
		var v any
		if err := json.Unmarshal(row.Result, &v); err == nil {
			m["result"] = v
		}
	}
	line := fmt.Sprintf("devloop %s: %s", row.Kind, row.State)
	if row.Detail != nil {
		line += " (" + *row.Detail + ")"
	}
	m["text"] = line
	return Result{Text: line, Structured: m}
}
