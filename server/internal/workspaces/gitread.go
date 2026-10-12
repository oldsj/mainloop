package workspaces

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/gittransport"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// GitRead gives a workspace's Session a git read capability: the workspace is enrolled for its
// repository before the Session is created, and the Session carries the reference to the
// capability's Secret so its git fetches through the read Service. A nil *GitRead leaves
// workspaces without one. The capability is revoked with the workspace by the credential service.
//
// Whether a create carries the reference is decided once, from the configuration in force when the
// create is first attempted, and frozen on the workspace row before the first CreateSession RPC.
// Every later attempt (a retry, or the cleanup replay) sends the frozen reference or none, whatever
// the configuration has become: kagent refuses a replayed request id whose credentials differ.
type GitRead struct {
	Enroller *gittransport.Enroller
	// Origin is the read Service's origin, where the agent's egress sends the capability.
	Origin string
}

// applies says whether the workspace would get a capability now: only a GitHub repository can be
// served.
func (g *GitRead) applies(w wsRow) bool {
	if g == nil || g.Enroller == nil {
		return false
	}
	_, err := github.ParseRepo(w.RepoURL)
	return err == nil
}

// frozenReference decodes the saved decision: nil, for a workspace that gets no git read.
func frozenReference(raw json.RawMessage) (*kagentapi.SessionCredential, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var ref kagentapi.SessionCredential
	if err := protojson.Unmarshal(raw, &ref); err != nil {
		return nil, fmt.Errorf("workspaces: stored git read credential: %w", err)
	}
	return &ref, nil
}

// gitReadForCreate returns the reference the workspace's create carries, deciding and freezing it on the
// first attempt. A workspace that already has its decision uses it, whatever the configuration is
// now. The workspace is enrolled (idempotent, so a retried create enrolls once) when a reference is
// sent and the enroller is configured; nil when the create carries none.
func (s *Service) gitReadForCreate(ctx context.Context, w wsRow) (*kagentapi.SessionCredential, error) {
	q := dbq.New(s.Pool)
	decided, raw := w.GitReadDecided, w.GitReadCredential
	if !decided {
		var want json.RawMessage
		if s.GitRead.applies(w) {
			b, err := protojson.Marshal(s.GitRead.reference(w))
			if err != nil {
				return nil, fmt.Errorf("workspaces: encode git read credential: %w", err)
			}
			want = b
		}
		// Frozen before the enrollment and the RPC. The update matches only an undecided
		// workspace, so a concurrent or earlier decision stands and is read back.
		row, err := q.FreezeWorkspaceGitRead(ctx, dbq.FreezeWorkspaceGitReadParams{ID: w.ID, GitReadCredential: want})
		switch {
		case err == nil:
			raw = row.GitReadCredential
		case errors.Is(err, pgx.ErrNoRows):
			cur, err := q.GetWorkspaceForOp(ctx, w.ID)
			if err != nil {
				return nil, err
			}
			raw = cur.GitReadCredential
		default:
			return nil, err
		}
	}
	ref, err := frozenReference(raw)
	if err != nil || ref == nil {
		return nil, err
	}
	if s.GitRead != nil && s.GitRead.Enroller != nil {
		if _, err := s.GitRead.Enroller.Enroll(ctx, w.ID); err != nil {
			return nil, err
		}
	}
	return ref, nil
}

// gitReadForReplay returns the reference the original create carried, issuing and enrolling
// nothing. The create was attempted, so its decision was frozen first; an undecided workspace has
// sent nothing and carries none.
func gitReadForReplay(w wsRow) (*kagentapi.SessionCredential, error) {
	return frozenReference(w.GitReadCredential)
}

func (g *GitRead) reference(w wsRow) *kagentapi.SessionCredential {
	return gittransport.Reference(g.Enroller.Creds, g.Origin, w.ID)
}
