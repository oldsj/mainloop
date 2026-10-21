package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/devloop"
)

// devLoopWaitBound is how long each tool call waits for its devloop_operations row to finish
// before answering with whatever is known so far; the row itself (and serve-api's reconcile loop)
// keeps working past this bound.
const devLoopWaitBound = 4 * time.Minute

// devLoopTools let a task agent whose project has a staging target configured (see
// docs/specs/devloop.md) iterate on it: one FIFO lease per target, a sync that fetches a pushed
// commit and copies it into the target's synced containers, and a read-only pod status. The MCP
// listener never reaches Kubernetes itself; every tool but the lease ones writes a
// devloop_operations row and polls it, and serve-api's reconcile loop (the only holder of a
// Kubernetes client) does the work.
func (s *Server) devLoopTools() []Tool {
	none := objectSchema(map[string]any{})
	sha := map[string]any{"type": "string", "pattern": "^[0-9a-f]{40}$"}
	return []Tool{
		{
			Name: agentauth.ToolDevLeaseAcquire,
			Description: "Acquire the one live-sync lease on your project's staging target, or report your place " +
				"in its FIFO queue. An idle-expired lease is taken over by the earliest queued session. A queued " +
				"session must call this again within 2 minutes to keep its place.",
			InputSchema: none, Handle: s.devLeaseAcquire,
		},
		{
			Name:        agentauth.ToolDevLeaseRelease,
			Description: "Release your project's staging-target lease if this session holds it.",
			InputSchema: none, Handle: s.devLeaseRelease,
		},
		{
			Name: agentauth.ToolDevSync,
			Description: "Use after pushing and before open_pull_request. Syncs commit_sha (already pushed through " +
				"the git push transport) into your project's staging target, migrates its database, waits for its " +
				"health, and runs its read-only smoke check. Succeeds only when the target is healthy and the " +
				"smoke passes; any other result is failed, with the output that explains it. Requires holding the " +
				"lease. Waits up to 4 minutes, then reports the state so far; retrying with the same commit_sha is safe.",
			InputSchema: objectSchema(map[string]any{"commit_sha": sha}, "commit_sha"), Handle: s.devSync,
		},
		{
			Name:        agentauth.ToolDevStatus,
			Description: "Read your project's staging-target pod status: phase, readiness and restart counts.",
			InputSchema: none, Handle: s.devStatus,
		},
	}
}

// devLoopAuthority fails closed unless the actor's project has a staging target configured
// (projects.ProjectsAPI's owner-only devloop-target routes are the only way to set one).
func (s *Server) devLoopAuthority(ctx context.Context, a agentauth.Actor) (devloop.Target, error) {
	if a.ProjectID == "" {
		return devloop.Target{}, failure("forbidden", "this session has no project")
	}
	if s.DevTargets == nil {
		return devloop.Target{}, failure("unavailable", "devloop is not available")
	}
	target, err := s.DevTargets.Get(ctx, a.ProjectID)
	if errors.Is(err, devloop.ErrTargetNotFound) {
		return devloop.Target{}, failure("forbidden", "this project has no staging target configured")
	}
	if err != nil {
		return devloop.Target{}, err
	}
	return target, nil
}

func (s *Server) devLeaseAcquire(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	if err := decode(raw, &struct{}{}); err != nil {
		return Result{}, err
	}
	target, err := s.devLoopAuthority(ctx, a)
	if err != nil {
		return Result{}, err
	}
	if s.DevLease == nil {
		return Result{}, failure("unavailable", "devloop is not available")
	}
	info, err := s.DevLease.Acquire(ctx, target.ID, a.BindingID)
	if err != nil {
		return Result{}, err
	}
	return leaseResult(info), nil
}

func (s *Server) devLeaseRelease(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	if err := decode(raw, &struct{}{}); err != nil {
		return Result{}, err
	}
	target, err := s.devLoopAuthority(ctx, a)
	if err != nil {
		return Result{}, err
	}
	if s.DevLease == nil {
		return Result{}, failure("unavailable", "devloop is not available")
	}
	if err := s.DevLease.Release(ctx, target.ID, a.BindingID); err != nil {
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
	target, err := s.devLoopAuthority(ctx, a)
	if err != nil {
		return Result{}, err
	}
	if s.DevLease == nil || s.DevOps == nil {
		return Result{}, failure("unavailable", "devloop is not available")
	}
	// Using the lease is also its heartbeat, so a session that syncs steadily never idles out.
	if err := s.DevLease.Use(ctx, target.ID, a.BindingID); err != nil {
		if errors.Is(err, devloop.ErrNotHolder) {
			return Result{}, failure("lease_required", "acquire the staging-target lease with dev_lease_acquire first")
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
	if _, err := s.devLoopAuthority(ctx, a); err != nil {
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
