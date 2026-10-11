package workspaces

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

const (
	sweepBatch = 100
	// opRetention is how long finished operations are kept for diagnosis.
	opRetention = 24 * time.Hour
)

// SuspendIdle queues a suspend for every workspace that has been quiet for its idle timeout. The
// suspend operation re-checks activity against quiet_since right before it calls kagent, so a
// message that arrives after this pass picked the workspace keeps it running.
func (s *Service) SuspendIdle(ctx context.Context) error {
	q := dbq.New(s.Pool)
	if _, err := q.PruneWorkspaceOps(ctx, opRetention.Seconds()); err != nil {
		s.Log.WarnContext(ctx, "could not prune workspace operations", "error", err)
	}
	cands, err := q.ListIdleWorkspaceCandidates(ctx, sweepBatch)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range cands {
		w := wsRow{ID: c.ID}
		if open, err := s.turnOpen(ctx, w); err != nil {
			errs = append(errs, err)
			continue
		} else if open {
			continue
		}
		activity, err := s.activity(ctx, w)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if s.now().Sub(activity) < time.Duration(c.IdleTimeoutMinutes)*time.Minute {
			continue
		}
		if _, err := s.enqueue(ctx, c.ID, dbstate.OpSuspend, &activity); err != nil && !errors.Is(err, ErrBusy) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// DeleteArchived deletes the kagent Session of every archived session and marks the binding.
// The binding columns are the durable intent: a failed or unconfirmed delete leaves it pending
// and the next pass retries, so a Session is never forgotten. An archived workspace with no
// recorded Session id gets a delete operation, which finds the Session its create may have made.
func (s *Service) DeleteArchived(ctx context.Context) error {
	q := dbq.New(s.Pool)
	rows, err := q.ListArchivedUndeletedBindings(ctx, sweepBatch)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range rows {
		if r.KagentSessionID == nil {
			continue // no Session id: ListArchivedWorkspacesWithoutSession handles these below
		}
		if open, err := s.turnOpen(ctx, wsRow{ID: r.SessionID}); err != nil {
			errs = append(errs, err)
			continue
		} else if open {
			continue // the next pass deletes it once the turn ends
		}
		if err := s.confirmDeleted(ctx, *r.KagentSessionID); err != nil {
			s.Log.WarnContext(ctx, "archived session not deleted yet", "session", r.SessionID, "error", err)
			errs = append(errs, err)
			continue
		}
		if err := q.MarkBindingKagentDeleted(ctx, r.SessionID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(append(errs, s.deleteArchivedWithoutSession(ctx, q))...)
}

// deleteArchivedWithoutSession queues the delete of every archived workspace whose create was sent
// but whose Session id was never recorded, once its create operation is closed. The delete replays
// the create to find the Session a lost reply may have made, deletes it and purges the rows; until
// then the binding counts as an unresolved create and the orphan sweep refuses to delete anything.
// A workspace whose create is still being retried is left to that operation.
func (s *Service) deleteArchivedWithoutSession(ctx context.Context, q *dbq.Queries) error {
	ids, err := q.ListArchivedWorkspacesWithoutSession(ctx, sweepBatch)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if open, err := s.turnOpen(ctx, wsRow{ID: id}); err != nil {
			errs = append(errs, err)
			continue
		} else if open {
			continue
		}
		if _, err := s.enqueue(ctx, id, dbstate.OpDelete, nil); err != nil && !errors.Is(err, ErrBusy) && !errors.Is(err, ErrNotFound) {
			s.Log.WarnContext(ctx, "could not queue the delete of an archived workspace", "workspace", id, "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// confirmDeleted deletes a kagent Session and returns nil only when kagent shows it gone: the same
// id in state DELETED, or not found.
func (s *Service) confirmDeleted(ctx context.Context, id string) error {
	sess, err := s.Kagent.DeleteSession(ctx, id)
	switch {
	case notFoundErr(err):
		return nil
	case err != nil:
		return err
	case sess.GetId() != id || sess.GetState() != kagentapi.RuntimeState_RUNTIME_STATE_DELETED:
		return &unconfirmed{"kagent did not confirm the delete"}
	}
	return nil
}

// OrphanOptions selects what an orphan pass does.
type OrphanOptions struct {
	// Delete removes the orphans; the default only lists them.
	Delete bool
}

// OrphanReport is what an orphan pass found and did.
type OrphanReport struct {
	// Orphans are the Sessions Mainloop created that no binding names.
	Orphans []string
	// Deleted are the ones confirmed gone.
	Deleted []string
	// Failed maps an orphan to why it was not deleted.
	Failed map[string]string
	// Blocked is why deletion was refused, empty when it was not.
	Blocked string
}

// SweepOrphans finds kagent Sessions that Mainloop created and no longer references, and with
// Delete removes them. Only Sessions created by this deployment's kagent user, on the configured
// provider agents, and older than the minimum age are considered, so another tenant's Sessions and
// a create still being recorded are never touched. The creator is a constant for every instance
// of Mainloop on this kagent, so the Agent names are what keep instances apart (see ownAgents). Deletion is refused while any binding has a
// create whose Session id is unknown, since that Session would look like an orphan.
func (s *Service) SweepOrphans(ctx context.Context, opt OrphanOptions) (OrphanReport, error) {
	rep := OrphanReport{Failed: map[string]string{}}
	sessions, err := s.Kagent.ListSessions(ctx)
	if err != nil {
		return rep, fmt.Errorf("list kagent sessions: %w", err)
	}
	q := dbq.New(s.Pool)
	bound, err := q.ListBoundKagentSessionIDs(ctx)
	if err != nil {
		return rep, err
	}
	boundSet := map[string]bool{}
	for _, id := range bound {
		if id != nil {
			boundSet[*id] = true
		}
	}
	ours, err := s.ownAgents(ctx)
	if err != nil {
		return rep, err
	}
	cutoff := s.now().Add(-s.Config.Workspaces.OrphanMinAge.Duration())
	for _, sess := range sessions {
		id := sess.GetId()
		if id == "" || boundSet[id] || sess.GetState() == kagentapi.RuntimeState_RUNTIME_STATE_DELETED {
			continue
		}
		if sess.GetCreator() != s.Config.Kagent.UserID || !ours[agentKey(sess.GetAgent())] {
			continue
		}
		if at := sess.GetCreatedAt(); at == nil || at.AsTime().After(cutoff) {
			continue // too new to judge, or no creation time to judge by
		}
		rep.Orphans = append(rep.Orphans, id)
	}
	slices.Sort(rep.Orphans)
	if !opt.Delete || len(rep.Orphans) == 0 {
		return rep, nil
	}
	var errs []error
	for _, id := range rep.Orphans {
		// Re-check right before each delete: a create may have bound it, or started, since the listing.
		if n, err := q.CountUnresolvedBindings(ctx); err != nil {
			return rep, err
		} else if n > 0 {
			rep.Blocked = fmt.Sprintf("%d binding(s) have a create with no recorded Session id; deleting could remove a Session that is about to be bound", n)
			return rep, nil
		}
		if _, err := q.GetAgentBindingByKagentSession(ctx, &id); err == nil {
			continue // bound since the listing
		} else if !isNoRows(err) {
			return rep, err
		}
		if err := s.confirmDeleted(ctx, id); err != nil {
			rep.Failed[id] = err.Error()
			errs = append(errs, fmt.Errorf("delete orphan %s: %w", id, err))
			continue
		}
		rep.Deleted = append(rep.Deleted, id)
	}
	return rep, errors.Join(errs...)
}

func agentKey(r *kagentapi.ResourceReference) string { return r.GetNamespace() + "/" + r.GetName() }

// ownAgents is every Agent this deployment's Sessions can be on: the configured provider Agents
// and those recorded on workspaces, which may predate a config change.
//
// The creator does not tell Mainloop instances apart: kagent records every Session made with the
// control token as created by the same constant user. Only distinct Agent names keep one
// instance's sweep away from another's Sessions, so instances sharing a kagent must not share
// Agents.
func (s *Service) ownAgents(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	for _, p := range s.Providers.Profiles() {
		for _, ref := range p.Agents {
			out[ref.Namespace+"/"+ref.Name] = true
		}
	}
	rows, err := dbq.New(s.Pool).ListWorkspaceAgents(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.AgentNamespace+"/"+r.AgentName] = true
	}
	return out, nil
}
