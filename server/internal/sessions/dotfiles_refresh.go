package sessions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/dotfiles"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1"
)

// Why a dotfiles refresh failed.
const (
	// RefreshNoBundle: the source gave no bundle; the pin's status and reason say why.
	RefreshNoBundle = "no_bundle"
	// RefreshNotDeliverable: the Session cannot download a bundle from Mainloop.
	RefreshNotDeliverable = "not_deliverable"
	// RefreshSessionGone: the native Session the refresh targeted no longer runs.
	RefreshSessionGone = "session_gone"
	// RefreshUnsupported: kagent cannot update a Session's dotfiles.
	RefreshUnsupported = "unsupported"
)

// refreshPinPrefix keys a refresh's pin in session_dotfiles: "refresh:<refresh id>".
const refreshPinPrefix = "refresh:"

// refreshCallTimeout bounds the kagent calls of one refresh attempt.
const refreshCallTimeout = 30 * time.Second

// DotfilesRefresh is one refresh of a session's agent dotfiles. Dotfiles is the pin it chose, nil
// until it has pinned.
type DotfilesRefresh struct {
	ID        string
	State     string
	Reason    string
	Dotfiles  *dotfiles.Pinned
	CreatedAt time.Time
}

// DotfilesInfo is what a session's dotfiles are and could be: the pin its native Session holds
// (Current, when one was recorded), its source now, whether a refresh would change them, and its
// latest refresh.
type DotfilesInfo struct {
	Current     *dotfiles.Pinned
	Source      *dotfiles.Head
	Behind      bool
	Refreshable bool
	Refresh     *DotfilesRefresh
}

// RefreshDotfiles gives the session's running native Session the dotfiles of its source's current
// commit, keeping the conversation: kagent lays them out before the Session's next turn and restarts
// a CLI that outlives turns between them (oldsj/kagent#112). An auto-follow setting would call it
// between turns when [DotfilesInfo.Behind]; nothing else is needed.
//
// It follows the reconciler rule. The intent is a session_dotfiles_refreshes row keyed by requestID,
// written under the session's send lock before anything else, recording the Session it updates.
// The pin (GitHub read, export) and the kagent calls run with no transaction open. Each attempt reads
// the Session from kagent first: one that holds the dotfiles already is applied, and the update is a
// compare-and-swap on the digest the Session was read with, so a repeat never applies twice and a
// Session that changed in between is read again. Repeating requestID returns the recorded outcome
// or continues a pending refresh; *KagentError is an attempt kagent did not answer, which a repeat
// or the resume sweep continues.
func (s *Service) RefreshDotfiles(ctx context.Context, sessionID, requestID string) (DotfilesRefresh, error) {
	if _, err := uuid.Parse(requestID); err != nil {
		return DotfilesRefresh{}, fmt.Errorf("%w: the request id is not a UUID", ErrInvalid)
	}
	if s.Kagent == nil {
		return DotfilesRefresh{}, &KagentError{Err: errors.New("kagent is not configured")}
	}
	if s.Dotfiles == nil {
		return DotfilesRefresh{}, conflict("Agent dotfiles are not configured on this server.")
	}
	r, err := s.beginRefresh(ctx, sessionID, requestID)
	if err != nil {
		return DotfilesRefresh{}, err
	}
	return s.continueRefresh(ctx, r)
}

// beginRefresh returns the refresh for requestID, writing it when it is new. A session that already
// has a pending refresh continues that one.
func (s *Service) beginRefresh(ctx context.Context, sessionID, requestID string) (dbq.SessionDotfilesRefresh, error) {
	var out dbq.SessionDotfilesRefresh
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := db.XactLock(ctx, tx, "mainloop:send:"+sessionID); err != nil {
			return err
		}
		q := dbq.New(tx)
		b, err := q.GetOwnedSessionBinding(ctx, dbq.GetOwnedSessionBindingParams{ID: sessionID, UserID: s.OwnerID})
		if errors.Is(err, pgx.ErrNoRows) {
			if sess, serr := q.GetSession(ctx, sessionID); serr == nil && sess.UserID == s.OwnerID {
				return conflict("Session has no native agent binding")
			}
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get binding: %w", err)
		}
		if r, err := q.GetDotfilesRefresh(ctx, requestID); err == nil {
			if r.SessionID != sessionID {
				return fmt.Errorf("%w: the request id belongs to another session", ErrInvalid)
			}
			out = r
			return nil // a repeat: its eligibility was checked when it was written
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("get refresh: %w", err)
		}
		if err := refreshable(b); err != nil {
			return err
		}
		if r, err := q.GetPendingDotfilesRefresh(ctx, sessionID); err == nil {
			out = r
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("get pending refresh: %w", err)
		}
		out, err = q.InsertDotfilesRefresh(ctx, dbq.InsertDotfilesRefreshParams{
			ID: requestID, SessionID: sessionID, KagentSessionID: *b.KagentSessionID,
		})
		if err != nil {
			return fmt.Errorf("record refresh: %w", err)
		}
		return nil
	})
	return out, err
}

// refreshable is nil when RefreshDotfiles applies to the binding's session.
func refreshable(b dbq.GetOwnedSessionBindingRow) error {
	switch {
	case b.ArchivedAt != nil:
		return conflict("This session is archived; start a new one.")
	case endedStatus(b.SessionStatus):
		return conflict("This session is %s; start a new one.", b.SessionStatus)
	case b.WorkspaceAgentName != nil:
		return conflict("A workspace session keeps the dotfiles it was created with; the next one gets the current ones.")
	case b.Role == dbstate.RoleMain && b.MainPending:
		return conflictCode(codePending, "The main thread is switching to %s; its new native session gets the current dotfiles.", b.Kind)
	case b.KagentSessionID == nil || *b.KagentSessionID == "" || b.KagentDeletedAt != nil:
		return conflict("This session has no native session running; the next one is created with the current dotfiles.")
	}
	return nil
}

// isRefreshable reports whether RefreshDotfiles applies to the session, for its dotfiles view.
func (s *Service) isRefreshable(ctx context.Context, sessionID string) (bool, error) {
	if s.Kagent == nil || s.Dotfiles == nil {
		return false, nil
	}
	b, err := dbq.New(s.Pool).GetOwnedSessionBinding(ctx, dbq.GetOwnedSessionBindingParams{ID: sessionID, UserID: s.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return refreshable(b) == nil, nil
}

// continueRefresh pins the refresh's dotfiles, unless it has, and has kagent hold them. A settled
// refresh returns its recorded outcome.
func (s *Service) continueRefresh(ctx context.Context, r dbq.SessionDotfilesRefresh) (DotfilesRefresh, error) {
	if r.State != dbstate.DotfilesRefreshPending {
		return s.refreshResult(ctx, r)
	}
	pin, err := s.Dotfiles.Pin(ctx, r.SessionID, refreshPinPrefix+r.ID, s.MCPOrigin)
	if err != nil {
		return DotfilesRefresh{}, err
	}
	var reason string
	switch {
	case pin.Status != dotfiles.StatusReady:
		reason = RefreshNoBundle
	case pin.URL == "":
		reason = RefreshNotDeliverable
	default:
		reason, err = s.updateNativeDotfiles(ctx, r.KagentSessionID, pin.Request())
	}
	if err != nil {
		detail := safeDetail(err.Error())
		if nerr := dbq.New(s.Pool).NoteDotfilesRefreshAttempt(ctx, dbq.NoteDotfilesRefreshAttemptParams{
			ID: r.ID, Detail: &detail,
		}); nerr != nil {
			err = errors.Join(err, nerr)
		}
		s.log().WarnContext(ctx, "dotfiles refresh: kagent did not answer", "session_id", r.SessionID,
			"kagent_session_id", r.KagentSessionID, "error", err)
		return DotfilesRefresh{}, &KagentError{Err: err}
	}
	state := dbstate.DotfilesRefreshApplied
	if reason != "" {
		state = dbstate.DotfilesRefreshFailed
	}
	settled, err := dbq.New(s.Pool).SettleDotfilesRefresh(ctx, dbq.SettleDotfilesRefreshParams{
		ID: r.ID, State: state, Reason: reason,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Settled by a concurrent caller: return what it recorded.
		settled, err = dbq.New(s.Pool).GetDotfilesRefresh(ctx, r.ID)
	}
	if err != nil {
		return DotfilesRefresh{}, fmt.Errorf("settle refresh: %w", err)
	}
	s.log().InfoContext(ctx, "dotfiles refresh settled", "session_id", r.SessionID, "state", settled.State,
		"reason", settled.Reason, "commit", pin.Commit)
	return s.refreshResult(ctx, settled)
}

// updateNativeDotfiles makes the Session hold want. It reads the Session first: one that is gone
// fails the refresh, one that holds want already is done. The update is a compare-and-swap on the
// digest just read, so a Session whose dotfiles changed in between is read again. A refusal is a
// failure reason; an error is an answer kagent did not give.
func (s *Service) updateNativeDotfiles(ctx context.Context, ksid string, want *kagentapi.Dotfiles) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, refreshCallTimeout)
	defer cancel()
	for range 3 {
		sess, err := s.Kagent.GetSession(ctx, ksid)
		switch {
		case nativeAbsent(err):
			return RefreshSessionGone, nil
		case err != nil:
			return "", fmt.Errorf("read the kagent session: %w", err)
		case sessionEnded(sess):
			return RefreshSessionGone, nil
		case proto.Equal(sess.GetDotfiles(), want):
			return "", nil
		}
		_, err = s.Kagent.UpdateSessionDotfiles(ctx, ksid, want, sess.GetDotfiles().GetDigest())
		switch code, wire := wireCode(err); {
		case err == nil:
			return "", nil
		case !wire:
			return "", fmt.Errorf("update the kagent session's dotfiles: %w", err)
		case code == connect.CodeNotFound:
			return RefreshSessionGone, nil
		case code == connect.CodeInvalidArgument:
			return RefreshNotDeliverable, nil
		case code == connect.CodeUnimplemented:
			return RefreshUnsupported, nil
		case code != connect.CodeFailedPrecondition:
			return "", fmt.Errorf("update the kagent session's dotfiles: %w", err)
		}
	}
	return "", errors.New("the kagent session's dotfiles changed during each attempt")
}

// wireCode is the status kagent itself sent, if err is one.
func wireCode(err error) (connect.Code, bool) {
	var se *kagent.SessionError
	if errors.As(err, &se) && se.Wire {
		return se.Code, true
	}
	return 0, false
}

// refreshResult is r with the pin it chose.
func (s *Service) refreshResult(ctx context.Context, r dbq.SessionDotfilesRefresh) (DotfilesRefresh, error) {
	out := DotfilesRefresh{ID: r.ID, State: string(r.State), Reason: r.Reason, CreatedAt: r.CreatedAt}
	pin, ok, err := dotfiles.Recorded(ctx, dbq.New(s.Pool), refreshPinPrefix+r.ID)
	if err != nil {
		return DotfilesRefresh{}, err
	}
	if ok {
		out.Dotfiles = &pin
	}
	return out, nil
}

// SessionDotfiles returns the session's dotfiles: what its native Session holds, its source now
// (read from GitHub at most once per [dotfiles.HeadTTL]), and its latest refresh.
func (s *Service) SessionDotfiles(ctx context.Context, sessionID string) (DotfilesInfo, error) {
	q := dbq.New(s.Pool)
	if _, err := q.GetSessionRow(ctx, dbq.GetSessionRowParams{ID: sessionID, UserID: s.OwnerID}); errors.Is(err, pgx.ErrNoRows) {
		return DotfilesInfo{}, ErrNotFound
	} else if err != nil {
		return DotfilesInfo{}, fmt.Errorf("get session: %w", err)
	}
	var out DotfilesInfo
	cur, ok, err := dotfiles.Latest(ctx, q, sessionID)
	if err != nil {
		return DotfilesInfo{}, fmt.Errorf("read the pin: %w", err)
	}
	if ok {
		out.Current = &cur
	}
	if s.Dotfiles != nil {
		head, err := s.Dotfiles.Head(ctx, sessionID)
		if err != nil {
			return DotfilesInfo{}, err
		}
		if head.Repository != "" {
			out.Source = &head
		}
		out.Behind = head.Behind(cur, ok)
	}
	if out.Refreshable, err = s.isRefreshable(ctx, sessionID); err != nil {
		return DotfilesInfo{}, err
	}
	r, err := q.LatestDotfilesRefresh(ctx, sessionID)
	switch {
	case err == nil:
		res, err := s.refreshResult(ctx, r)
		if err != nil {
			return DotfilesInfo{}, err
		}
		out.Refresh = &res
	case !errors.Is(err, pgx.ErrNoRows):
		return DotfilesInfo{}, fmt.Errorf("read the latest refresh: %w", err)
	}
	return out, nil
}

// refreshIdle is how long a pending refresh is left alone before the sweep continues it, so the
// sweep does not race the request that is still running it.
const refreshIdle = time.Minute

// DotfilesRefreshSweep continues pending refreshes that were cut off or that kagent did not answer.
func (s *Service) DotfilesRefreshSweep() reconcile.Sweep {
	return reconcile.Sweep{Name: "resume-dotfiles-refreshes", Every: 30 * time.Second, Run: s.ResumeDotfilesRefreshes}
}

// ResumeDotfilesRefreshes runs one pass of [Service.DotfilesRefreshSweep].
func (s *Service) ResumeDotfilesRefreshes(ctx context.Context) error {
	if s.Kagent == nil || s.Dotfiles == nil {
		return nil
	}
	rows, err := dbq.New(s.Pool).ListDueDotfilesRefreshes(ctx, dbq.ListDueDotfilesRefreshesParams{
		IdleSeconds: refreshIdle.Seconds(), LimitCount: 20,
	})
	if err != nil {
		return fmt.Errorf("list due refreshes: %w", err)
	}
	var errs []error
	for _, r := range rows {
		if _, err := s.continueRefresh(ctx, r); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
