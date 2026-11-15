package dotfiles

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

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
}

// Pin returns the dotfiles of the native Session that create request requestID makes for
// sessionID, choosing them on first use: the project's source when it has one, else the owner's;
// the commit its ref names now; and that commit's export.
//
// It is called before the create is sent, outside any transaction. GitHub is read first and the
// pin is written after, keyed by the request: a retried or replayed create gets the pin its first
// attempt recorded, and two concurrent pins of one request settle on whichever was written first.
// Only a database failure is an error; a source that cannot be read gives a pin without a bundle,
// and the Session runs without dotfiles.
func (e *Exports) Pin(ctx context.Context, sessionID, requestID string) (Pinned, error) {
	if requestID == "" {
		return Pinned{}, errors.New("dotfiles: pin needs a create request id")
	}
	q := dbq.New(e.Pool)
	if p, ok, err := e.pinned(ctx, q, requestID); err != nil || ok {
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
	if err := q.InsertPinnedDotfiles(ctx, dbq.InsertPinnedDotfilesParams{
		RequestID: requestID, SessionID: sessionID,
		Repository: p.Repository, Ref: p.Ref, Path: p.Path,
		Commit: optional(p.Commit), Digest: optional(p.Digest), Status: p.Status, Reason: p.Reason,
	}); err != nil {
		return Pinned{}, fmt.Errorf("dotfiles: record the pin: %w", err)
	}
	p, ok, err := e.pinned(ctx, q, requestID)
	if err == nil && !ok {
		err = errors.New("dotfiles: the pin was not recorded")
	}
	return p, err
}

// Latest returns the pin of sessionID's most recent native Session, if it has one.
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

func (e *Exports) pinned(ctx context.Context, q *dbq.Queries, requestID string) (Pinned, bool, error) {
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
	return p
}
