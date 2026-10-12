package sessions

import (
	"context"
	"errors"
	"fmt"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// ReadySession makes a session's kagent Session ready to take a send, by the rule deliveries use:
// a workspace session wakes its workspace first (through the Waker), and a suspended Session is
// resumed. It waits for a Session that is still starting, up to Options.ReadyWait. It sends
// nothing. workspaceID is the session's workspace, nil for a session that has none.
//
// kagent refuses a send to any Session that is not ready, so a caller that sends on its own (the
// HITL dispatcher) must call this first and treat an error as "not sent yet, try again later".
func (s *Service) ReadySession(ctx context.Context, sessionID string, workspaceID *string, kagentSessionID string) error {
	if kagentSessionID == "" {
		return errors.New("the session has no kagent session")
	}
	t := &turn{s: s, ksid: kagentSessionID, d: dbq.GetDeliveryContextRow{SessionID: sessionID, WorkspaceID: workspaceID}}
	if err := t.checkSendFence(ctx); err != nil {
		return err
	}
	if workspaceID != nil {
		if err := t.workspacePrepared(ctx); err != nil {
			return err
		}
	}
	if workspaceID != nil && s.Waker != nil {
		if err := s.Waker.Wake(ctx, *workspaceID); err != nil {
			return fmt.Errorf("wake the workspace: %w", err)
		}
	}
	if err := t.checkSendFence(ctx); err != nil {
		return err
	}
	sess, err := s.Kagent.GetSession(ctx, kagentSessionID)
	if err != nil {
		return err
	}
	if sessionEnded(sess) {
		return errors.New("the kagent session is being deleted")
	}
	_, err = t.readyKagentSession(ctx, sess)
	return err
}
