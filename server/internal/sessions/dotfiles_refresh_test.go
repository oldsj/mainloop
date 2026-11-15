package sessions_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/dotfiles"
	"github.com/oldsj/mainloop/server/internal/dotfiles/dotfilestest"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/sessions"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1"
)

// Fictional: the source's next commit and its export.
var (
	nextBundle = []byte("fictional dotfiles bundle, next commit")
	nextCommit = strings.Repeat("e", 40)
)

// refreshEnv is a running main thread created with the pinner's dotfiles, its session id and its
// native Session's id. ready says whether the source had a bundle when it was created.
func refreshEnv(t *testing.T, ready bool) (*env, *dotfilestest.Pinner, string, string) {
	t.Helper()
	e := newEnv(t)
	e.withCredentials()
	pins := &dotfilestest.Pinner{Pool: e.pool, Ready: ready}
	e.svc.Dotfiles = pins
	e.run()
	e.waitState(e.send("hello"), dbstate.DeliveryCompleted)
	m, err := e.svc.EnsureMain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := dbq.New(e.pool).GetOwnedSessionBinding(context.Background(), dbq.GetOwnedSessionBindingParams{ID: m.SessionID, UserID: owner})
	if err != nil || b.KagentSessionID == nil {
		t.Fatalf("binding = %+v, %v", b, err)
	}
	return e, pins, m.SessionID, *b.KagentSessionID
}

func (e *env) dotfilesInfo(sessionID string) api.SessionDotfilesInfo {
	e.t.Helper()
	resp, err := e.API().GetSessionDotfiles(context.Background(), api.GetSessionDotfilesRequestObject{SessionId: sessionID})
	if err != nil {
		e.t.Fatal(err)
	}
	return api.SessionDotfilesInfo(resp.(api.GetSessionDotfiles200JSONResponse))
}

func (e *env) refreshDotfiles(sessionID, requestID string) (api.SessionDotfilesRefresh, error) {
	e.t.Helper()
	resp, err := e.API().RefreshSessionDotfiles(context.Background(), api.RefreshSessionDotfilesRequestObject{
		SessionId: sessionID, Body: &api.SessionDotfilesRefreshRequest{RequestId: uuid.MustParse(requestID)},
	})
	if err != nil {
		return api.SessionDotfilesRefresh{}, err
	}
	return api.SessionDotfilesRefresh(resp.(api.RefreshSessionDotfiles200JSONResponse)), nil
}

func kagenttestFault(code connect.Code) kagenttest.Fault { return kagenttest.Fault{GRPC: &code} }

func nextDotfiles() *kagentapi.Dotfiles {
	d := dotfilestest.DigestOf(nextBundle)
	return &kagentapi.Dotfiles{
		Url: mcpOrigin + dotfiles.BundlePath + d, Digest: d,
		Repository: dotfilestest.Repository, Commit: nextCommit, Path: dotfilestest.Path,
	}
}

// When the source moves on, the session shows it is behind; a refresh gives the running native
// Session the new commit's bundle, compared against the digest it held, and the session then
// shows the new commit. Repeating the request returns the outcome without calling kagent again.
func TestARefreshGivesTheMainThreadTheSourcesNewCommit(t *testing.T) {
	e, pins, id, ksid := refreshEnv(t, true)
	if info := e.dotfilesInfo(id); info.Behind || !info.Refreshable || info.Current == nil ||
		*info.Current.Commit != dotfilestest.Commit || info.Source == nil || *info.Source.Commit != dotfilestest.Commit {
		t.Fatalf("before the source moves: %+v", info)
	}
	pins.Advance(nextBundle, nextCommit)
	if info := e.dotfilesInfo(id); !info.Behind || *info.Source.Commit != nextCommit || *info.Current.Commit != dotfilestest.Commit {
		t.Fatalf("after the source moved: %+v", info)
	}

	req := uuid.NewString()
	r, err := e.refreshDotfiles(id, req)
	if err != nil {
		t.Fatal(err)
	}
	if r.Id != req || r.State != api.SessionDotfilesRefreshStateApplied || r.Reason != "" ||
		r.Dotfiles == nil || *r.Dotfiles.Commit != nextCommit {
		t.Fatalf("refresh = %+v", r)
	}
	updates := e.srv.DotfileUpdates()
	if len(updates) != 1 || updates[0].GetSessionId() != ksid || updates[0].GetPreviousDigest() != dotfilestest.Digest() ||
		!proto.Equal(updates[0].GetDotfiles(), nextDotfiles()) {
		t.Fatalf("updates = %v", updates)
	}
	if sess, _ := e.srv.Session(ksid); !proto.Equal(sess.GetDotfiles(), nextDotfiles()) {
		t.Fatalf("kagent holds %v", sess.GetDotfiles())
	}
	info := e.dotfilesInfo(id)
	if info.Behind || *info.Current.Commit != nextCommit || info.Refresh == nil || info.Refresh.Id != req ||
		info.Refresh.State != api.SessionDotfilesRefreshStateApplied {
		t.Fatalf("after the refresh: %+v", info)
	}

	again, err := e.refreshDotfiles(id, req)
	if err != nil || again.State != api.SessionDotfilesRefreshStateApplied || len(e.srv.DotfileUpdates()) != 1 {
		t.Fatalf("repeat = %+v, %v; %d updates", again, err, len(e.srv.DotfileUpdates()))
	}
	// The conversation goes on in the same native Session.
	e.waitState(e.send("again"), dbstate.DeliveryCompleted)
	if n := len(e.srv.CreateRequests()); n != 1 {
		t.Fatalf("%d creates, want the one Session kept", n)
	}
}

// A main thread created while its source had no bundle holds no dotfiles in kagent; a refresh once
// the source has one gives it them.
func TestARefreshGivesDotfilesToASessionCreatedWithout(t *testing.T) {
	e, pins, id, ksid := refreshEnv(t, false)
	if info := e.dotfilesInfo(id); info.Behind || info.Source != nil || info.Current.Status != api.SessionDotfilesStatusNone {
		t.Fatalf("without a source: %+v", info)
	}
	pins.Ready = true
	pins.Advance(nextBundle, nextCommit)
	if info := e.dotfilesInfo(id); !info.Behind {
		t.Fatalf("with a source: %+v", info)
	}
	r, err := e.refreshDotfiles(id, uuid.NewString())
	if err != nil || r.State != api.SessionDotfilesRefreshStateApplied {
		t.Fatalf("refresh = %+v, %v", r, err)
	}
	if u := e.srv.DotfileUpdates(); len(u) != 1 || u[0].GetPreviousDigest() != "" {
		t.Fatalf("updates = %v", u)
	}
	if sess, _ := e.srv.Session(ksid); !proto.Equal(sess.GetDotfiles(), nextDotfiles()) {
		t.Fatalf("kagent holds %v", sess.GetDotfiles())
	}
}

// A refresh that cannot apply fails with its reason and leaves what the session holds alone.
func TestARefreshThatCannotApplyFailsWithItsReason(t *testing.T) {
	t.Run("no bundle", func(t *testing.T) {
		e, pins, id, _ := refreshEnv(t, true)
		pins.Ready = false
		r, err := e.refreshDotfiles(id, uuid.NewString())
		if err != nil || r.State != api.SessionDotfilesRefreshStateFailed || r.Reason != api.SessionDotfilesRefreshReasonNoBundle ||
			r.Dotfiles == nil || r.Dotfiles.Status != api.SessionDotfilesStatusNone {
			t.Fatalf("refresh = %+v, %v", r, err)
		}
		if info := e.dotfilesInfo(id); *info.Current.Commit != dotfilestest.Commit || len(e.srv.DotfileUpdates()) != 0 {
			t.Fatalf("after: %+v", info)
		}
	})
	t.Run("no credential to download it", func(t *testing.T) {
		e, pins, id, _ := refreshEnv(t, true)
		pins.Advance(nextBundle, nextCommit)
		e.svc.MCPOrigin = ""
		r, err := e.refreshDotfiles(id, uuid.NewString())
		if err != nil || r.State != api.SessionDotfilesRefreshStateFailed || r.Reason != api.SessionDotfilesRefreshReasonNotDeliverable {
			t.Fatalf("refresh = %+v, %v", r, err)
		}
	})
	t.Run("kagent refuses the url", func(t *testing.T) {
		e, pins, id, _ := refreshEnv(t, true)
		pins.Advance(nextBundle, nextCommit)
		e.svc.MCPOrigin = "http://elsewhere.test"
		r, err := e.refreshDotfiles(id, uuid.NewString())
		if err != nil || r.State != api.SessionDotfilesRefreshStateFailed || r.Reason != api.SessionDotfilesRefreshReasonNotDeliverable {
			t.Fatalf("refresh = %+v, %v", r, err)
		}
		if info := e.dotfilesInfo(id); !info.Behind || *info.Current.Commit != dotfilestest.Commit {
			t.Fatalf("after: %+v", info)
		}
	})
	t.Run("an older kagent", func(t *testing.T) {
		e, pins, id, _ := refreshEnv(t, true)
		pins.Advance(nextBundle, nextCommit)
		e.srv.FailNext(kagent.ProcUpdateSessionDotfiles, kagenttestFault(connect.CodeUnimplemented))
		r, err := e.refreshDotfiles(id, uuid.NewString())
		if err != nil || r.State != api.SessionDotfilesRefreshStateFailed || r.Reason != api.SessionDotfilesRefreshReasonUnsupported {
			t.Fatalf("refresh = %+v, %v", r, err)
		}
	})
	t.Run("the native session is gone", func(t *testing.T) {
		e, pins, id, ksid := refreshEnv(t, true)
		pins.Advance(nextBundle, nextCommit)
		if _, err := e.svc.Kagent.DeleteSession(context.Background(), ksid); err != nil {
			t.Fatal(err)
		}
		r, err := e.refreshDotfiles(id, uuid.NewString())
		if err != nil || r.State != api.SessionDotfilesRefreshStateFailed || r.Reason != api.SessionDotfilesRefreshReasonSessionGone {
			t.Fatalf("refresh = %+v, %v", r, err)
		}
	})
}

// An update kagent did not answer leaves the refresh pending and is a 502; repeating the request
// continues it, and so does the sweep once the refresh has been left alone.
func TestAnUnansweredRefreshIsContinued(t *testing.T) {
	for _, via := range []string{"repeat", "sweep"} {
		t.Run(via, func(t *testing.T) {
			e, pins, id, ksid := refreshEnv(t, true)
			pins.Advance(nextBundle, nextCommit)
			e.srv.FailNext(kagent.ProcUpdateSessionDotfiles, kagenttestFault(connect.CodeUnavailable))
			req := uuid.NewString()
			if _, err := e.refreshDotfiles(id, req); err == nil || !strings.Contains(err.Error(), "refresh") {
				t.Fatalf("an unanswered refresh = %v", err)
			}
			if info := e.dotfilesInfo(id); info.Refresh == nil || info.Refresh.State != api.SessionDotfilesRefreshStatePending ||
				*info.Current.Commit != dotfilestest.Commit {
				t.Fatalf("while pending: %+v", info)
			}
			// Another request while one is pending continues that one.
			other := uuid.NewString()
			var r api.SessionDotfilesRefresh
			var err error
			switch via {
			case "repeat":
				r, err = e.refreshDotfiles(id, other)
			case "sweep":
				if _, err := e.pool.Exec(context.Background(),
					`UPDATE session_dotfiles_refreshes SET updated_at = now() - interval '2 minutes' WHERE id = $1`, req); err != nil {
					t.Fatal(err)
				}
				err = e.svc.ResumeDotfilesRefreshes(context.Background())
				r = *e.dotfilesInfo(id).Refresh
			}
			if err != nil || r.Id != req || r.State != api.SessionDotfilesRefreshStateApplied {
				t.Fatalf("continued = %+v, %v", r, err)
			}
			if sess, _ := e.srv.Session(ksid); !proto.Equal(sess.GetDotfiles(), nextDotfiles()) {
				t.Fatalf("kagent holds %v", sess.GetDotfiles())
			}
		})
	}
}

// A refresh applies only to a session with a running native Session that is not a workspace's.
func TestARefreshNeedsARunningNativeSession(t *testing.T) {
	e := newEnv(t)
	e.withCredentials()
	e.svc.Dotfiles = &dotfilestest.Pinner{Pool: e.pool, Ready: true}
	m, err := e.svc.EnsureMain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info := e.dotfilesInfo(m.SessionID); info.Refreshable {
		t.Fatalf("refreshable before a native session: %+v", info)
	}
	_, err = e.svc.RefreshDotfiles(context.Background(), m.SessionID, uuid.NewString())
	if !errors.Is(err, sessions.ErrConflict) {
		t.Fatalf("refresh before a native session = %v", err)
	}
	if _, err := e.svc.RefreshDotfiles(context.Background(), "no-such-session", uuid.NewString()); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("refresh of a missing session = %v", err)
	}
	if _, err := e.svc.RefreshDotfiles(context.Background(), m.SessionID, "not-a-uuid"); !errors.Is(err, sessions.ErrInvalid) {
		t.Fatalf("refresh with a bad request id = %v", err)
	}
}
