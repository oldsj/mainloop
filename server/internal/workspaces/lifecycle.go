package workspaces

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// wsRow is a workspace with its binding's kagent Session id.
type wsRow dbq.GetWorkspaceForOpRow

func (w wsRow) sessionID() string {
	if w.KagentSessionID == nil {
		return ""
	}
	return *w.KagentSessionID
}

func (w wsRow) ports() []api.WorkspacePort {
	out := []api.WorkspacePort{}
	_ = json.Unmarshal(w.Ports, &out)
	return out
}

func (w wsRow) environment() *api.WorkspaceEnvironment {
	if len(w.Environment) == 0 {
		return nil
	}
	var e api.WorkspaceEnvironment
	if json.Unmarshal(w.Environment, &e) != nil {
		return nil
	}
	return &e
}

// stateOf maps a kagent Session to the observed state the API shows, with a detail when kagent
// gave a reason.
func stateOf(sess *kagentapi.Session) (api.WorkspaceObservedState, *string) {
	starting := "Starting."
	switch {
	case sess.GetState() == kagentapi.RuntimeState_RUNTIME_STATE_FAILED:
		detail := sess.GetFailure().GetMessage()
		if detail == "" {
			detail = sess.GetFailure().GetReason()
		}
		if detail == "" {
			detail = "The workspace failed."
		}
		return api.WorkspaceObservedStateFailed, &detail
	case sess.GetState() == kagentapi.RuntimeState_RUNTIME_STATE_DELETING,
		sess.GetState() == kagentapi.RuntimeState_RUNTIME_STATE_DELETED:
		return api.WorkspaceObservedStateUnknown, nil
	case sess.GetState() == kagentapi.RuntimeState_RUNTIME_STATE_CREATING,
		sess.GetOperation() == kagentapi.RuntimeOperation_RUNTIME_OPERATION_CREATE,
		sess.GetOperation() == kagentapi.RuntimeOperation_RUNTIME_OPERATION_RESUME:
		return api.WorkspaceObservedStateResuming, &starting
	case sess.GetOperation() == kagentapi.RuntimeOperation_RUNTIME_OPERATION_SUSPEND:
		return api.WorkspaceObservedStateSuspending, nil
	case sess.GetState() == kagentapi.RuntimeState_RUNTIME_STATE_SUSPENDED:
		return api.WorkspaceObservedStateSuspended, nil
	case sess.GetState() == kagentapi.RuntimeState_RUNTIME_STATE_READY:
		return api.WorkspaceObservedStateRunning, nil
	}
	return api.WorkspaceObservedStateUnknown, nil
}

// project builds the contract lifecycle. sess is what kagent reported, nil when there is no
// Session or it could not be read; create is the newest create operation, nil if none. mode and
// reason are the workspace's actual push authority, from [publicationOf].
func project(w wsRow, sess *kagentapi.Session, create *dbq.WorkspaceOp, delivery time.Time, mode api.WorkspaceLifecyclePublicationMode, reason *api.WorkspaceLifecyclePublicationReason) api.WorkspaceLifecycle {
	state, detail := api.WorkspaceObservedStateUnknown, (*string)(nil)
	switch {
	case sess != nil:
		state, detail = stateOf(sess)
	case w.KagentSessionID == nil && create != nil:
		switch create.State {
		case dbstate.OpRejected, dbstate.OpGaveUp:
			msg := "Creating the workspace stopped"
			if create.LastError != nil {
				msg += ": " + *create.LastError
			}
			msg += ". Refresh to try again."
			state, detail = api.WorkspaceObservedStateFailed, &msg
		case dbstate.OpPending, dbstate.OpRetrying:
			state = api.WorkspaceObservedStateResuming
			msg := "Starting."
			if create.LastError != nil {
				msg = "Could not create the workspace yet (" + *create.LastError + "); retrying automatically."
			}
			detail = &msg
		}
	}
	active := w.LastActiveAt
	if !delivery.IsZero() && (active == nil || delivery.After(*active)) {
		active = &delivery
	}
	return api.WorkspaceLifecycle{
		Detail:         detail,
		LastActivityAt: active,
		Manifest: api.WorkspaceManifest{
			AgentKind: w.AgentKind, Branch: w.Branch, Depth: int(w.Depth),
			Dev: api.WorkspaceDevOutput{
				IdleTimeoutMinutes: int(w.IdleTimeoutMinutes), Ports: w.ports(),
			},
			DevelopmentEnvironment: w.environment(),
			Ref:                    w.Ref, RepoUrl: w.RepoURL,
		},
		ObservedState:     state,
		PublicationMode:   mode,
		PublicationReason: reason,
		SessionId:         w.ID,
		UpdatedAt:         w.UpdatedAt,
		WorkspaceId:       w.ID,
	}
}

// previewURL is the router-facing URL of a workspace port: <port>--<workspace>--preview.<host>.
func previewURL(base, workspaceID string, port int) string {
	u, err := url.Parse(base)
	host, scheme := "localhost", "http"
	netloc := ""
	if err == nil {
		if h := u.Hostname(); h != "" {
			host = h
		}
		if u.Scheme != "" {
			scheme = u.Scheme
		}
		if p := u.Port(); p != "" {
			netloc = ":" + p
		}
	}
	return fmt.Sprintf("%s://%d--%s--preview.%s%s", scheme, port, workspaceID, host, netloc)
}

// previewPorts lists a workspace's ports by number with their preview URLs.
func previewPorts(base string, w wsRow) []api.WorkspacePreviewPort {
	ports := w.ports()
	sort.Slice(ports, func(i, j int) bool { return ports[i].Number < ports[j].Number })
	out := make([]api.WorkspacePreviewPort, 0, len(ports))
	for _, p := range ports {
		out = append(out, api.WorkspacePreviewPort{Name: p.Name, Port: p.Number, Url: previewURL(base, w.ID, p.Number)})
	}
	return out
}
