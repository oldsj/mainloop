package workspaces

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/gittransport"
	"github.com/oldsj/mainloop/server/internal/kagent"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// landedUnrecorded makes a workspace whose first create reached kagent, and was saved there, but
// whose reply Mainloop could not use: the Session exists and no binding records it. The git read
// capability is on or off as asked while the create is sent.
func landedUnrecorded(t *testing.T, gitOn bool) (*harness, *authtest.Env, api.WorkspaceLifecycle, func()) {
	t.Helper()
	h := newHarness(t, slow)
	a := h.withCredentials()
	gr := &GitRead{Enroller: &gittransport.Enroller{Pool: h.svc.Pool, Creds: a.Creds}, Origin: h.cfg.Git.ReadOrigin}
	if gitOn {
		h.svc.GitRead = gr
	}
	var blank atomic.Bool
	blank.Store(true)
	prev := h.fake.TamperSession
	h.fake.TamperSession = func(proc string, s *kagentapi.Session) {
		prev(proc, s)
		if proc == kagent.ProcCreateSession && blank.Load() {
			s.Id = ""
		}
	}
	stop := h.run()
	w, err := h.create(api.CreateWorkspaceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the unusable reply", func() bool { return h.op(w.WorkspaceId, "create").Attempts > 0 && len(h.sessions()) == 1 })
	stop()
	if row := h.row(w.WorkspaceId); row.KagentSessionID != nil {
		t.Fatalf("the binding is recorded: %v", *row.KagentSessionID)
	}
	enable := func() { // the process restarts with git read in the other state
		if gitOn {
			h.svc.GitRead = nil
		} else {
			h.svc.GitRead = gr
		}
		blank.Store(false)
	}
	return h, a, w, enable
}

// The credential set a create carries is frozen with the workspace, so a restart that changes
// whether git read is configured cannot turn the retry into a request kagent refuses.
func TestAnUncertainCreateKeepsItsCredentialSetWhenGitReadConfigurationChanges(t *testing.T) {
	for name, gitOn := range map[string]bool{"on-to-off": true, "off-to-on": false} {
		t.Run(name, func(t *testing.T) {
			h, a, w, flip := landedUnrecorded(t, gitOn)
			id := w.WorkspaceId
			row := h.row(id)
			if !row.GitReadDecided || (row.GitReadCredential != nil) != gitOn {
				t.Fatalf("frozen decision = %v, %s", row.GitReadDecided, row.GitReadCredential)
			}
			orig := h.fake.CreateRequests()
			if len(orig) != 1 || (len(gitReadRefs(orig[0].GetCredentials())) == 1) != gitOn {
				t.Fatalf("original credentials = %v", orig)
			}
			issued := len(h.credentialRows(id))
			flip()

			stop := h.run()
			defer stop()
			h.due()
			eventually(t, "the retry binds the Session", func() bool { return h.row(id).KagentSessionID != nil })
			if op := h.op(id, "create"); op.State != dbstate.OpSucceeded {
				t.Fatalf("create op = %s (%v)", op.State, op.LastError)
			}
			reqs := h.fake.CreateRequests()
			if len(reqs) != 2 || len(h.sessions()) != 1 {
				t.Fatalf("creates = %d, sessions = %d", len(reqs), len(h.sessions()))
			}
			if !credentialsEqual(reqs[0].GetCredentials(), reqs[1].GetCredentials()) {
				t.Errorf("retry credentials = %v, original %v", reqs[1].GetCredentials(), reqs[0].GetCredentials())
			}
			if gitOn {
				if got := len(h.credentialRows(id)); got != issued {
					t.Errorf("credential rows %d -> %d: the retry issued something", issued, got)
				}
			} else if len(gitReadRefs(reqs[1].GetCredentials())) != 0 {
				t.Errorf("the retry added a git read reference")
			}
			if _, err := h.dq.GetGitEnrollment(bg, id); (err == nil) != gitOn {
				t.Errorf("enrollment present = %v, want %v", err == nil, gitOn)
			}
			_ = a

			h.svc.Now = func() time.Time { return time.Now().Add(time.Hour) }
			rep, err := h.svc.SweepOrphans(bg, OrphanOptions{Delete: true})
			if err != nil || rep.Blocked != "" || len(rep.Orphans) != 0 {
				t.Fatalf("orphan sweep: %+v, %v", rep, err)
			}
			if _, alive := h.fake.Session(*h.row(id).KagentSessionID); !alive {
				t.Error("the bound Session was deleted")
			}
		})
	}
}

// The cleanup replay of an uncertain create sends the frozen credential set too, so kagent finds
// the Session and the delete removes it, whatever the configuration became.
func TestADeleteReplayKeepsTheCreatesCredentialSetWhenGitReadConfigurationChanges(t *testing.T) {
	for name, gitOn := range map[string]bool{"on-to-off": true, "off-to-on": false} {
		t.Run(name, func(t *testing.T) {
			h, _, w, flip := landedUnrecorded(t, gitOn)
			id := w.WorkspaceId
			issued := len(h.credentialRows(id))
			flip()

			h.svc.Config.Workspaces.OperationWait = 5e9
			stop := h.run()
			defer stop()
			if err := h.svc.Delete(bg, id); err != nil {
				t.Fatal(err)
			}
			reqs := h.fake.CreateRequests()
			if len(reqs) != 2 || !credentialsEqual(reqs[0].GetCredentials(), reqs[1].GetCredentials()) {
				t.Fatalf("requests = %v", reqs)
			}
			if h.workspaceCount() != 0 || len(h.sessions()) != 0 {
				t.Errorf("workspaces = %d, sessions = %d: the landed Session was not deleted", h.workspaceCount(), len(h.sessions()))
			}
			if gitOn && issued != 2 {
				t.Errorf("credentials issued before the flip = %d", issued)
			}
		})
	}
}

func credentialsEqual(a, b []*kagentapi.SessionCredential) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].GetOrigin() != b[i].GetOrigin() || a[i].GetHeader() != b[i].GetHeader() ||
			a[i].GetSecretRef().GetName() != b[i].GetSecretRef().GetName() || a[i].GetSecretRef().GetKey() != b[i].GetSecretRef().GetKey() {
			return false
		}
	}
	return true
}

// A workspace that gets no git read says so on its row, and the decision does not change when the
// capability is configured later.
func TestAWorkspaceWithoutGitReadFreezesThatDecision(t *testing.T) {
	h := newHarness(t, nil)
	a := h.withCredentials()
	w := h.running()
	row := h.row(w.WorkspaceId)
	if !row.GitReadDecided || row.GitReadCredential != nil {
		t.Fatalf("decision = %v, %s", row.GitReadDecided, row.GitReadCredential)
	}
	h.svc.GitRead = &GitRead{Enroller: &gittransport.Enroller{Pool: h.svc.Pool, Creds: a.Creds}, Origin: h.cfg.Git.ReadOrigin}
	if ref, err := gitReadForReplay(h.row(w.WorkspaceId)); ref != nil || err != nil {
		t.Errorf("replay reference = %v, %v", ref, err)
	}
}
