package dotfiles

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1"
)

// BundlePath is where Mainloop's MCP listener serves a bundle, by digest.
const BundlePath = "/dotfiles/"

// Pinned is the dotfiles one native Session was created with: the source that applied, the commit
// its ref named then, and the bundle it was given. Status says whether there is a bundle; Reason
// says why not.
type Pinned struct {
	Repository string
	Ref        string
	Path       string
	Commit     string
	Digest     string
	Status     string
	Reason     string
	// URL is where the create sent the bundle to be fetched from; empty when it sent none.
	URL string
}

// Request is the dotfiles the create sends to kagent, or nil when it sends none.
func (p Pinned) Request() *kagentapi.Dotfiles {
	if p.URL == "" {
		return nil
	}
	return &kagentapi.Dotfiles{Url: p.URL, Digest: p.Digest, Repository: p.Repository, Commit: p.Commit, Path: p.Path}
}

// Sent returns the dotfiles create request requestID sent, for a replay of it, which sends the
// same. Nil when the request has no pin, or its pin sent none.
func Sent(ctx context.Context, q *dbq.Queries, requestID string) (*kagentapi.Dotfiles, error) {
	p, _, err := pinned(ctx, q, requestID)
	if err != nil {
		return nil, fmt.Errorf("dotfiles: read the pin: %w", err)
	}
	return p.Request(), nil
}

// Pin returns the dotfiles of the native Session that create request requestID makes for
// sessionID, choosing them on first use: the project's source when it has one, else the owner's;
// the commit its ref names now; and that commit's export. mcpOrigin is the origin of the MCP
// credential the create carries, which authorizes the runtime's download of the bundle from
// Mainloop's MCP listener; empty when the create carries none, and then it sends no dotfiles.
//
// It is called before the create is sent, outside any transaction. GitHub is read first and the
// pin is written after, keyed by the request: a retried or replayed create gets the pin its first
// attempt recorded, and two concurrent pins of one request settle on whichever was written first.
// Only a database failure is an error; a source that cannot be read gives a pin without a bundle,
// and the Session runs without dotfiles.
func (e *Exports) Pin(ctx context.Context, sessionID, requestID, mcpOrigin string) (Pinned, error) {
	if requestID == "" {
		return Pinned{}, errors.New("dotfiles: pin needs a create request id")
	}
	q := dbq.New(e.Pool)
	if p, ok, err := pinned(ctx, q, requestID); err != nil || ok {
		return p, err
	}
	src, err := e.effectiveSource(ctx, q, sessionID)
	if err != nil {
		return Pinned{}, err
	}
	p, err := e.choose(ctx, src)
	if err != nil {
		return Pinned{}, err
	}
	p, err = Record(ctx, q, sessionID, requestID, mcpOrigin, p)
	if err == nil {
		e.ownerExported(ctx, q, sessionID, src, p)
	}
	return p, err
}

// ownerExported gives OwnerExported the AGENTS.md of a ready pin of the owner's own source (a
// project's source is not the owner's dotfiles). It never fails the pin: a failure is logged.
func (e *Exports) ownerExported(ctx context.Context, q *dbq.Queries, sessionID string, src Source, p Pinned) {
	if e.OwnerExported == nil || p.Status != StatusReady || p.Digest == "" {
		return
	}
	in, err := q.GetSessionDotfilesSourceInputs(ctx, sessionID)
	if err != nil {
		e.log().WarnContext(ctx, "dotfiles: owner export check: read the session", "error", err)
		return
	}
	row, err := q.GetOwnerDotfilesSource(ctx, in.UserID)
	if err != nil || row.Repository == nil || !strings.EqualFold(*row.Repository, src.Repository) || row.Ref != src.Ref || row.Path != src.Path {
		return
	}
	bundle, ok, err := e.BundleContent(ctx, p.Digest)
	if err != nil || !ok {
		e.log().WarnContext(ctx, "dotfiles: owner export check: read the bundle", "found", ok, "error", err)
		return
	}
	agents, err := BundleFile(bundle, "AGENTS.md")
	if err != nil {
		e.log().WarnContext(ctx, "dotfiles: owner export check: read AGENTS.md", "error", err)
		return
	}
	if err := e.OwnerExported(ctx, in.UserID, p.Commit, agents); err != nil {
		e.log().WarnContext(ctx, "dotfiles: owner export check failed", "error", err)
	}
}

// Record writes p as the pin of create request requestID unless the request has one, and returns
// the pin the request holds. A ready pin is sent at its url on mcpOrigin, or not at all without
// one. p's bundle must already be stored.
func Record(ctx context.Context, q *dbq.Queries, sessionID, requestID, mcpOrigin string, p Pinned) (Pinned, error) {
	p.URL = ""
	if p.Status == StatusReady && mcpOrigin != "" {
		p.URL = strings.TrimRight(mcpOrigin, "/") + BundlePath + p.Digest
	}
	if err := q.InsertPinnedDotfiles(ctx, dbq.InsertPinnedDotfilesParams{
		RequestID: requestID, SessionID: sessionID,
		Repository: p.Repository, Ref: p.Ref, Path: p.Path,
		Commit: optional(p.Commit), Digest: optional(p.Digest), Status: p.Status, Reason: p.Reason,
		Url: optional(p.URL),
	}); err != nil {
		return Pinned{}, fmt.Errorf("dotfiles: record the pin: %w", err)
	}
	p, ok, err := pinned(ctx, q, requestID)
	if err == nil && !ok {
		err = errors.New("dotfiles: the pin was not recorded")
	}
	return p, err
}

// Recorded returns the pin of request requestID, if it has one.
func Recorded(ctx context.Context, q *dbq.Queries, requestID string) (Pinned, bool, error) {
	return pinned(ctx, q, requestID)
}

// Latest returns the pin sessionID's native Session holds, if one was recorded: its create's, or its
// latest applied refresh's when that is newer.
func Latest(ctx context.Context, q *dbq.Queries, sessionID string) (Pinned, bool, error) {
	row, err := q.LatestSessionDotfiles(ctx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Pinned{}, false, nil
	}
	if err != nil {
		return Pinned{}, false, err
	}
	return fromRow(row), true, nil
}

// choose reads src from GitHub: the commit, then the export. It writes no pin.
func (e *Exports) choose(ctx context.Context, src Source) (Pinned, error) {
	if src.Repository == "" {
		return Pinned{Status: StatusNone}, nil
	}
	p := Pinned{Repository: src.Repository, Ref: src.Ref, Path: src.Path}
	commit, unavailable := e.Resolve(ctx, src)
	if unavailable != nil {
		p.Status, p.Reason = unavailable.Status, unavailable.Reason
		return p, nil
	}
	r, err := e.Export(ctx, src, commit)
	if err != nil {
		return Pinned{}, err
	}
	// An unavailable export keeps the commit: the ref resolved, only its files could not be read.
	p.Commit, p.Status, p.Reason, p.Digest = commit, r.Status, r.Reason, r.Digest
	return p, nil
}

// effectiveSource is the session's project's source when that names a repository, else the owner's.
func (e *Exports) effectiveSource(ctx context.Context, q *dbq.Queries, sessionID string) (Source, error) {
	s, err := q.GetSessionDotfilesSourceInputs(ctx, sessionID)
	if err != nil {
		return Source{}, fmt.Errorf("dotfiles: read the session: %w", err)
	}
	if s.ProjectID != nil {
		row, err := q.GetProjectDotfilesSource(ctx, *s.ProjectID)
		switch {
		case err == nil && row.Repository != nil:
			return Source{Repository: *row.Repository, Ref: row.Ref, Path: row.Path}, nil
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return Source{}, fmt.Errorf("dotfiles: read the project source: %w", err)
		}
	}
	row, err := q.GetOwnerDotfilesSource(ctx, s.UserID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Source{}, nil
	case err != nil:
		return Source{}, fmt.Errorf("dotfiles: read the owner source: %w", err)
	case row.Repository == nil:
		return Source{}, nil
	}
	return Source{Repository: *row.Repository, Ref: row.Ref, Path: row.Path}, nil
}

func pinned(ctx context.Context, q *dbq.Queries, requestID string) (Pinned, bool, error) {
	row, err := q.GetPinnedDotfiles(ctx, requestID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Pinned{}, false, nil
	}
	if err != nil {
		return Pinned{}, false, fmt.Errorf("dotfiles: read the pin: %w", err)
	}
	return fromRow(row), true, nil
}

func fromRow(row dbq.SessionDotfile) Pinned {
	p := Pinned{Repository: row.Repository, Ref: row.Ref, Path: row.Path, Status: row.Status, Reason: row.Reason}
	if row.Commit != nil {
		p.Commit = *row.Commit
	}
	if row.Digest != nil {
		p.Digest = *row.Digest
	}
	if row.Url != nil {
		p.URL = *row.Url
	}
	return p
}
