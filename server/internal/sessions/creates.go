package sessions

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// createsBatch bounds how many unresolved creates one pass looks at.
const createsBatch = 50

// ResolveCreatesSweep settles the creates of sessions that have nothing open any more. A create
// whose reply never arrived may have made a kagent Session Mainloop never recorded, and until it
// is settled the orphan sweep refuses to delete anything. The create is idempotent by request id,
// so replaying it shows what kagent holds: no Session (the request id is reserved by a deleted one)
// settles it at once, and a Session that comes back is deleted first, then the create is settled.
// A later send to the same session then finds the request id deleted and replaces it.
func (s *Service) ResolveCreatesSweep() reconcile.Sweep {
	return reconcile.Sweep{
		Name: "resolve-session-creates", Every: s.opts().ResolveEvery,
		Run: s.ResolveCreates,
	}
}

// ResolveCreates runs one pass of [Service.ResolveCreatesSweep]. A create it cannot settle now
// (kagent is down, no provider is configured for the session) stays unresolved and is tried again.
func (s *Service) ResolveCreates(ctx context.Context) error {
	if s.Kagent == nil {
		return nil
	}
	q := dbq.New(s.Pool)
	rows, err := q.ListUnresolvedCreates(ctx, createsBatch)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range rows {
		if err := s.resolveCreate(ctx, q, r); err != nil {
			s.log().WarnContext(ctx, "could not settle a session create", "session_id", r.SessionID, "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Service) resolveCreate(ctx context.Context, q *dbq.Queries, r dbq.ListUnresolvedCreatesRow) error {
	agent, err := s.bindingAgent(&r.Kind, r.Role)
	if err != nil {
		return fmt.Errorf("resolve agent: %w", err)
	}
	sess, err := s.Kagent.CreateSession(ctx, &kagentapi.CreateSessionRequest{
		Agent:     &kagentapi.ResourceReference{Namespace: agent.Namespace, Name: agent.Name},
		RequestId: r.CreateRequestID,
		Name:      "Mainloop session",
	})
	var se *kagent.SessionError
	switch {
	case errors.As(err, &se) && se.Code == connect.CodeFailedPrecondition:
		// The request id belongs to a Session that is gone, or the Agent is not ready: kagent looked
		// the id up first and found no Session holding it, so nothing is left to find.
	case err != nil:
		return fmt.Errorf("replay the create: %w", err)
	case sess.GetState() != kagentapi.RuntimeState_RUNTIME_STATE_DELETED:
		if err := s.deleteConfirmed(ctx, sess.GetId()); err != nil {
			return err
		}
	}
	_, err = q.ResolveCreate(ctx, dbq.ResolveCreateParams{SessionID: r.SessionID, CreateRequestID: &r.CreateRequestID})
	return err
}

// deleteConfirmed deletes a kagent Session and returns nil only when kagent shows it gone.
func (s *Service) deleteConfirmed(ctx context.Context, id string) error {
	sess, err := s.Kagent.DeleteSession(ctx, id)
	var se *kagent.SessionError
	switch {
	case errors.As(err, &se) && se.Code == connect.CodeNotFound:
		return nil
	case err != nil:
		return fmt.Errorf("delete the Session the create returned: %w", err)
	case sess.GetId() != id || sess.GetState() != kagentapi.RuntimeState_RUNTIME_STATE_DELETED:
		return fmt.Errorf("kagent did not confirm the delete of %s (%s)", id, sess.GetState())
	}
	return nil
}
