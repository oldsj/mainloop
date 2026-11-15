package dotfiles

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// Task error reasons for the dotfiles routes.
const (
	reasonProjectNotFound = "project_not_found"
	reasonStaleSource     = "stale_dotfiles_source"
)

var (
	errProjectNotFound = errors.New("project not found")
	errStaleSource     = errors.New("stale dotfiles source")
)

// DotfilesAPI serves the owner-wide and per-project dotfiles source.
type DotfilesAPI struct {
	Pool *pgxpool.Pool
	// OwnerID is the one owner this instance serves; projects of anyone else are not found.
	OwnerID string
}

// NewAPI builds the API for the instance's one owner.
func NewAPI(pool *pgxpool.Pool, ownerID string) *DotfilesAPI {
	return &DotfilesAPI{Pool: pool, OwnerID: ownerID}
}

// GetDotfilesSource implements the contract operation. None set reads as repository null at
// version 0.
func (a *DotfilesAPI) GetDotfilesSource(ctx context.Context, _ api.GetDotfilesSourceRequestObject) (api.GetDotfilesSourceResponseObject, error) {
	row, err := dbq.New(a.Pool).GetOwnerDotfilesSource(ctx, a.OwnerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.GetDotfilesSource200JSONResponse(api.DotfilesSource{}), nil
	}
	if err != nil {
		return nil, err
	}
	return api.GetDotfilesSource200JSONResponse(toAPI(row.Repository, row.Ref, row.Path, row.Version)), nil
}

// SetDotfilesSource implements the contract operation: a compare-and-swap on the version.
func (a *DotfilesAPI) SetDotfilesSource(ctx context.Context, req api.SetDotfilesSourceRequestObject) (api.SetDotfilesSourceResponseObject, error) {
	src, err := fromUpdate(req.Body)
	if err != nil {
		return nil, apierr.Invalid(err.Error())
	}
	q := dbq.New(a.Pool)
	var row dbq.OwnerDotfilesSource
	if req.Body.ExpectedVersion == 0 {
		row, err = q.InsertOwnerDotfilesSource(ctx, dbq.InsertOwnerDotfilesSourceParams{
			OwnerID: a.OwnerID, Repository: optional(src.Repository), Ref: src.Ref, Path: src.Path,
		})
	} else {
		row, err = q.UpdateOwnerDotfilesSource(ctx, dbq.UpdateOwnerDotfilesSourceParams{
			OwnerID: a.OwnerID, Repository: optional(src.Repository), Ref: src.Ref, Path: src.Path,
			ExpectedVersion: int64(req.Body.ExpectedVersion),
		})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apierr.Reason(http.StatusConflict, reasonStaleSource)
	}
	if err != nil {
		return nil, err
	}
	return api.SetDotfilesSource200JSONResponse(toAPI(row.Repository, row.Ref, row.Path, row.Version)), nil
}

// GetProjectDotfilesSource implements the contract operation. None set reads as repository null
// at version 0, and the owner-wide source applies.
func (a *DotfilesAPI) GetProjectDotfilesSource(ctx context.Context, req api.GetProjectDotfilesSourceRequestObject) (api.GetProjectDotfilesSourceResponseObject, error) {
	q := dbq.New(a.Pool)
	if err := a.requireOwnedProject(ctx, q, req.ProjectId); err != nil {
		if errors.Is(err, errProjectNotFound) {
			return nil, apierr.Reason(http.StatusNotFound, reasonProjectNotFound)
		}
		return nil, err
	}
	row, err := q.GetProjectDotfilesSource(ctx, req.ProjectId)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.GetProjectDotfilesSource200JSONResponse(api.DotfilesSource{}), nil
	}
	if err != nil {
		return nil, err
	}
	return api.GetProjectDotfilesSource200JSONResponse(toAPI(row.Repository, row.Ref, row.Path, row.Version)), nil
}

// SetProjectDotfilesSource implements the contract operation, with the same rules as
// SetDotfilesSource.
func (a *DotfilesAPI) SetProjectDotfilesSource(ctx context.Context, req api.SetProjectDotfilesSourceRequestObject) (api.SetProjectDotfilesSourceResponseObject, error) {
	src, err := fromUpdate(req.Body)
	if err != nil {
		return nil, apierr.Invalid(err.Error())
	}
	var out api.DotfilesSource
	err = db.InTx(ctx, a.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if err := a.requireOwnedProject(ctx, q, req.ProjectId); err != nil {
			return err
		}
		var row dbq.ProjectDotfilesSource
		var err error
		if req.Body.ExpectedVersion == 0 {
			row, err = q.InsertProjectDotfilesSource(ctx, dbq.InsertProjectDotfilesSourceParams{
				ProjectID: req.ProjectId, Repository: optional(src.Repository), Ref: src.Ref, Path: src.Path,
			})
		} else {
			row, err = q.UpdateProjectDotfilesSource(ctx, dbq.UpdateProjectDotfilesSourceParams{
				ProjectID: req.ProjectId, Repository: optional(src.Repository), Ref: src.Ref, Path: src.Path,
				ExpectedVersion: int64(req.Body.ExpectedVersion),
			})
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return errStaleSource
		}
		if err != nil {
			return err
		}
		out = toAPI(row.Repository, row.Ref, row.Path, row.Version)
		return nil
	})
	switch {
	case errors.Is(err, errProjectNotFound):
		return nil, apierr.Reason(http.StatusNotFound, reasonProjectNotFound)
	case errors.Is(err, errStaleSource):
		return nil, apierr.Reason(http.StatusConflict, reasonStaleSource)
	case err != nil:
		return nil, err
	}
	return api.SetProjectDotfilesSource200JSONResponse(out), nil
}

// requireOwnedProject returns errProjectNotFound for a missing project and for one that belongs
// to another owner, so the two cannot be told apart.
func (a *DotfilesAPI) requireOwnedProject(ctx context.Context, q *dbq.Queries, id string) error {
	owner, err := q.GetProjectOwner(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != a.OwnerID) {
		return errProjectNotFound
	}
	return err
}

func fromUpdate(b *api.DotfilesSourceUpdate) (Source, error) {
	var s Source
	if b.Repository != nil {
		s.Repository = *b.Repository
	}
	if b.Ref != nil {
		s.Ref = *b.Ref
	}
	if b.Path != nil {
		s.Path = *b.Path
	}
	return Normalize(s)
}

func toAPI(repository *string, ref, path string, version int64) api.DotfilesSource {
	return api.DotfilesSource{Repository: repository, Ref: ref, Path: path, Version: int(version)}
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
