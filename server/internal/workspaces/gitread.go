package workspaces

import (
	"context"

	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/gittransport"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// GitRead gives a workspace's Session a git read capability: the workspace is enrolled for its
// repository before the Session is created, and the Session carries the reference to the
// capability's Secret so its git fetches through the read Service. A nil *GitRead leaves
// workspaces without one. The capability is revoked with the workspace by the credential service.
type GitRead struct {
	Enroller *gittransport.Enroller
	// Origin is the read Service's origin, where the agent's egress sends the capability.
	Origin string
}

// applies says whether the workspace gets a capability: only a GitHub repository can be served. It
// reads stored fields alone, so a replayed create decides as the original did.
func (g *GitRead) applies(w wsRow) bool {
	if g == nil || g.Enroller == nil {
		return false
	}
	_, err := github.ParseRepo(w.RepoURL)
	return err == nil
}

// forCreate enrolls the workspace (idempotent, so a retried create enrolls once) and returns the
// reference to add to its create request; nil when the workspace gets none.
func (g *GitRead) forCreate(ctx context.Context, w wsRow) (*kagentapi.SessionCredential, error) {
	if !g.applies(w) {
		return nil, nil
	}
	if _, err := g.Enroller.Enroll(ctx, w.ID); err != nil {
		return nil, err
	}
	return g.reference(w), nil
}

// forReplay returns the reference the original create carried, issuing and enrolling nothing: the
// Secret's name is a function of the workspace, and kagent refuses a replayed request id whose
// credentials differ from the original's.
func (g *GitRead) forReplay(w wsRow) *kagentapi.SessionCredential {
	if !g.applies(w) {
		return nil
	}
	return g.reference(w)
}

func (g *GitRead) reference(w wsRow) *kagentapi.SessionCredential {
	return gittransport.Reference(g.Enroller.Creds, g.Origin, w.ID)
}
