package environments

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/httpx"
)

// Service holds the environment rules. Every method takes the acting owner explicitly; the API
// passes the one configured owner. Failures the caller should see are [httpx.Error]s.
//
// Registry I/O never runs inside a transaction (the reconciler rule): refresh reads, validates
// and then writes in a separate transaction.
type Service struct {
	Pool      *pgxpool.Pool
	Validator *Validator
	// NewID returns a fresh identifier. Nil means 128 random bits, hex encoded.
	NewID func() string
}

func (s *Service) id() string {
	if s.NewID != nil {
		return s.NewID()
	}
	var b [16]byte
	// crypto/rand.Read never returns an error on supported platforms (Go 1.24+).
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func notFound(detail string) error { return httpx.NewError(http.StatusNotFound, detail) }

func unprocessable(detail string) error {
	return httpx.NewError(http.StatusUnprocessableEntity, detail)
}

// registryFailure turns a validator error into a 422; anything else (a database problem, the
// caller's cancellation) passes through.
func registryFailure(err error) error {
	var re *RegistryError
	if errors.As(err, &re) {
		return unprocessable(re.Message)
	}
	return err
}

// ownedEnvironment reads (or row-locks) an environment the owner owns. Another owner's
// environment is not found.
func ownedEnvironment(ctx context.Context, q *dbq.Queries, owner, id string, lock bool) (dbq.DevEnvironment, error) {
	get := q.GetDevEnvironment
	if lock {
		get = q.LockDevEnvironment
	}
	env, err := get(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && env.OwnerID != owner {
		return dbq.DevEnvironment{}, notFound("Environment not found")
	}
	return env, err
}

func environmentVersion(ctx context.Context, q *dbq.Queries, environmentID, versionID string) (dbq.EnvironmentVersion, error) {
	v, err := q.GetEnvironmentVersion(ctx, dbq.GetEnvironmentVersionParams{EnvironmentID: environmentID, ID: versionID})
	if errors.Is(err, pgx.ErrNoRows) {
		return v, notFound("Environment version not found")
	}
	return v, err
}

// Register validates the request, then stores the environment and its first version. An invalid
// registration persists nothing.
func (s *Service) Register(ctx context.Context, owner string, req api.RegisterEnvironment) (api.RegisteredEnvironment, error) {
	sourceKind := SourcePrebuiltImage
	if req.SourceKind != nil {
		sourceKind = string(*req.SourceKind)
	}
	architecture := "amd64"
	if req.Architecture != nil {
		architecture = string(*req.Architecture)
	}
	envID := s.id()
	env := dbq.InsertDevEnvironmentParams{ID: envID, OwnerID: owner, Name: req.Name, SourceKind: sourceKind, WatchedTag: req.WatchedTag}

	var version dbq.InsertEnvironmentVersionParams
	switch sourceKind {
	case SourcePrebuiltImage:
		if req.Image == nil || *req.Image == "" || req.Definition != nil {
			return api.RegisteredEnvironment{}, unprocessable("prebuilt registration requires image only")
		}
		var err error
		if version, err = s.Validator.Validate(ctx, *req.Image, architecture, envID, s.id()); err != nil {
			return api.RegisteredEnvironment{}, registryFailure(err)
		}
	case SourceDefinitionRepo:
		if req.Definition == nil || req.Image != nil || req.WatchedTag != nil {
			return api.RegisteredEnvironment{}, unprocessable("definition registration requires definition only")
		}
		if p := req.Definition.Path; strings.HasPrefix(p, "/") || containsDotDot(p) {
			return api.RegisteredEnvironment{}, unprocessable("definition path must stay within the repository")
		}
		definition, err := json.Marshal(req.Definition)
		if err != nil {
			return api.RegisteredEnvironment{}, err
		}
		env.Definition = definition
		version = dbq.InsertEnvironmentVersionParams{
			ID: s.id(), EnvironmentID: envID, Definition: definition,
			CapabilityProfile: json.RawMessage(`{}`), ValidationStatus: StatusPendingBuild,
			ValidationResult: json.RawMessage(`{"build":"not_run"}`), ValidatorVersion: ValidatorVersion,
			ProvenanceKind: ProvenanceMainloopBuilt, BaseProvenance: "unknown",
		}
	default:
		return api.RegisteredEnvironment{}, unprocessable("unsupported source kind")
	}

	var out api.RegisteredEnvironment
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		row, err := q.InsertDevEnvironment(ctx, env)
		if err != nil {
			return err
		}
		v, err := q.InsertEnvironmentVersion(ctx, version)
		if err != nil {
			return err
		}
		if out.Environment, err = environmentToAPI(row); err != nil {
			return err
		}
		out.Version, err = versionToAPI(v)
		return err
	})
	return out, err
}

func containsDotDot(path string) bool {
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

// List returns the owner's environments, oldest first.
func (s *Service) List(ctx context.Context, owner string) ([]api.DevEnvironment, error) {
	rows, err := dbq.New(s.Pool).ListDevEnvironments(ctx, owner)
	if err != nil {
		return nil, err
	}
	out := make([]api.DevEnvironment, len(rows))
	for i, row := range rows {
		if out[i], err = environmentToAPI(row); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Get returns one owned environment.
func (s *Service) Get(ctx context.Context, owner, id string) (api.DevEnvironment, error) {
	row, err := ownedEnvironment(ctx, dbq.New(s.Pool), owner, id, false)
	if err != nil {
		return api.DevEnvironment{}, err
	}
	return environmentToAPI(row)
}

// Versions lists an owned environment's versions in creation order.
func (s *Service) Versions(ctx context.Context, owner, id string) ([]api.EnvironmentVersion, error) {
	q := dbq.New(s.Pool)
	if _, err := ownedEnvironment(ctx, q, owner, id, false); err != nil {
		return nil, err
	}
	rows, err := q.ListEnvironmentVersions(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]api.EnvironmentVersion, len(rows))
	for i, row := range rows {
		if out[i], err = versionToAPI(row); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Version returns one version of an owned environment.
func (s *Service) Version(ctx context.Context, owner, id, versionID string) (api.EnvironmentVersion, error) {
	q := dbq.New(s.Pool)
	if _, err := ownedEnvironment(ctx, q, owner, id, false); err != nil {
		return api.EnvironmentVersion{}, err
	}
	row, err := environmentVersion(ctx, q, id, versionID)
	if err != nil {
		return api.EnvironmentVersion{}, err
	}
	return versionToAPI(row)
}

// Refresh resolves the environment's watched tag again and appends the result as a new immutable
// version. It never changes the accepted default or any project's selection. Every refresh adds
// a version, even when the tag still points at the same image.
func (s *Service) Refresh(ctx context.Context, owner, id, previousID string) (api.EnvironmentVersion, error) {
	q := dbq.New(s.Pool)
	env, err := ownedEnvironment(ctx, q, owner, id, false)
	if err != nil {
		return api.EnvironmentVersion{}, err
	}
	previous, err := environmentVersion(ctx, q, id, previousID)
	if err != nil {
		return api.EnvironmentVersion{}, err
	}
	// No transaction is open here: the registry can be slow.
	value, err := s.Validator.Refresh(ctx, env, previous, s.id())
	if err != nil {
		return api.EnvironmentVersion{}, registryFailure(err)
	}
	var out api.EnvironmentVersion
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if _, err := ownedEnvironment(ctx, q, owner, id, true); err != nil {
			return err
		}
		row, err := q.InsertEnvironmentVersion(ctx, value)
		if err != nil {
			return err
		}
		out, err = versionToAPI(row)
		return err
	})
	return out, err
}

// SetDefault makes a statically validated version the environment's accepted default.
func (s *Service) SetDefault(ctx context.Context, owner, id, versionID string) (api.DevEnvironment, error) {
	var out api.DevEnvironment
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if _, err := ownedEnvironment(ctx, q, owner, id, true); err != nil {
			return err
		}
		v, err := environmentVersion(ctx, q, id, versionID)
		if err != nil {
			return err
		}
		if v.ValidationStatus != StatusStaticValidated {
			return unprocessable("Version is not statically validated")
		}
		row, err := q.SetDevEnvironmentDefault(ctx, dbq.SetDevEnvironmentDefaultParams{ID: id, DefaultVersionID: &versionID})
		if err != nil {
			return err
		}
		out, err = environmentToAPI(row)
		return err
	})
	return out, err
}

// Grant lets a project use an owned environment. A repeated grant replaces the permission.
func (s *Service) Grant(ctx context.Context, owner, id, projectID, permission string) (api.ProjectEnvironmentGrant, error) {
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if err := lockForGrant(ctx, q, owner, id, projectID); err != nil {
			return err
		}
		_, err := q.UpsertEnvironmentGrant(ctx, dbq.UpsertEnvironmentGrantParams{EnvironmentID: id, ProjectID: projectID, Permission: permission})
		return err
	})
	if err != nil {
		return api.ProjectEnvironmentGrant{}, err
	}
	return api.ProjectEnvironmentGrant{EnvironmentId: id, ProjectId: projectID, Permission: api.ProjectEnvironmentGrantPermission(permission)}, nil
}

// Revoke removes a project's grant. Revoking a grant that does not exist succeeds.
func (s *Service) Revoke(ctx context.Context, owner, id, projectID string) error {
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if err := lockForGrant(ctx, q, owner, id, projectID); err != nil {
			return err
		}
		return q.DeleteEnvironmentGrant(ctx, dbq.DeleteEnvironmentGrantParams{EnvironmentID: id, ProjectID: projectID})
	})
}

// lockForGrant row-locks the owned environment, then requires the project to exist. Grants may
// name any project, not only the owner's.
func lockForGrant(ctx context.Context, q *dbq.Queries, owner, id, projectID string) error {
	if _, err := ownedEnvironment(ctx, q, owner, id, true); err != nil {
		return err
	}
	if _, err := q.GetProjectOwner(ctx, projectID); errors.Is(err, pgx.ErrNoRows) {
		return notFound("Project not found")
	} else if err != nil {
		return err
	}
	return nil
}

// ownedProject reads the project's owner, optionally locking the row. A project of another owner
// is not found. The lock is FOR NO KEY UPDATE so it serialises selection writers without
// blocking a grant insert's foreign-key check on the same project.
func ownedProject(ctx context.Context, q *dbq.Queries, owner, projectID string, lock bool) error {
	get := q.GetProjectOwner
	if lock {
		get = q.LockProjectForSelection
	}
	projectOwner, err := get(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && projectOwner != owner {
		return notFound("Project not found")
	}
	return err
}

func hasAccess(ctx context.Context, q *dbq.Queries, env dbq.DevEnvironment, owner, projectID string) (bool, error) {
	if env.OwnerID == owner {
		return true, nil
	}
	_, err := q.GetEnvironmentGrant(ctx, dbq.GetEnvironmentGrantParams{EnvironmentID: env.ID, ProjectID: projectID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Select points a project at an environment version, or at the environment's default. The
// request must carry the selection's current revision (0 for none); a stale one is a conflict.
func (s *Service) Select(ctx context.Context, owner, projectID string, req api.SelectEnvironment) (api.ProjectEnvironmentSelection, error) {
	follow := req.FollowDefault != nil && *req.FollowDefault
	if follow == (req.VersionId != nil) {
		return api.ProjectEnvironmentSelection{}, unprocessable("choose an explicit version or follow_default")
	}
	var out api.ProjectEnvironmentSelection
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		// The project lock serialises first selections and replacements; the environment lock
		// serialises default and grant changes with the access check and version resolution.
		if err := ownedProject(ctx, q, owner, projectID, true); err != nil {
			return err
		}
		env, err := q.LockDevEnvironment(ctx, req.EnvironmentId)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound("Environment not found")
		} else if err != nil {
			return err
		}
		if ok, err := hasAccess(ctx, q, env, owner, projectID); err != nil {
			return err
		} else if !ok {
			return httpx.NewError(http.StatusForbidden, "Environment use grant required")
		}
		var revision int64
		if current, err := q.GetEnvironmentSelection(ctx, projectID); err == nil {
			revision = current.Revision
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if revision != int64(req.ExpectedVersion) {
			return httpx.NewError(http.StatusConflict, "Stale expected version")
		}
		target := req.VersionId
		if follow {
			target = env.DefaultVersionID
		}
		if target == nil {
			return unprocessable("Environment has no accepted default")
		}
		candidate, err := environmentVersion(ctx, q, env.ID, *target)
		if err != nil {
			return err
		}
		if candidate.ValidationStatus != StatusStaticValidated {
			return unprocessable("Version is not statically validated")
		}
		if _, err := q.UpsertEnvironmentSelection(ctx, dbq.UpsertEnvironmentSelectionParams{
			ProjectID: projectID, EnvironmentID: env.ID, VersionID: req.VersionId, FollowDefault: follow, Revision: revision + 1,
		}); err != nil {
			return err
		}
		out, _, err = readSelection(ctx, q, owner, projectID)
		return err
	})
	return out, err
}

// Selection returns the project's selection and whether one exists.
func (s *Service) Selection(ctx context.Context, owner, projectID string) (api.ProjectEnvironmentSelection, bool, error) {
	q := dbq.New(s.Pool)
	if err := ownedProject(ctx, q, owner, projectID, false); err != nil {
		return api.ProjectEnvironmentSelection{}, false, err
	}
	return readSelection(ctx, q, owner, projectID)
}

func readSelection(ctx context.Context, q *dbq.Queries, owner, projectID string) (api.ProjectEnvironmentSelection, bool, error) {
	row, err := q.GetEnvironmentSelection(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.ProjectEnvironmentSelection{}, false, nil
	} else if err != nil {
		return api.ProjectEnvironmentSelection{}, false, err
	}
	env, err := q.GetDevEnvironment(ctx, row.EnvironmentID)
	if err != nil {
		return api.ProjectEnvironmentSelection{}, false, err
	}
	ok, err := hasAccess(ctx, q, env, owner, projectID)
	if err != nil {
		return api.ProjectEnvironmentSelection{}, false, err
	}
	resolved := row.VersionID
	if row.FollowDefault {
		resolved = env.DefaultVersionID
	}
	return api.ProjectEnvironmentSelection{
		ProjectId: projectID, EnvironmentId: row.EnvironmentID, VersionId: row.VersionID,
		FollowDefault: row.FollowDefault, Revision: int(row.Revision),
		AccessRevoked: !ok, ResolvedVersionId: resolved,
	}, true, nil
}

func environmentToAPI(e dbq.DevEnvironment) (api.DevEnvironment, error) {
	definition, err := decodeDefinition(e.Definition)
	if err != nil {
		return api.DevEnvironment{}, err
	}
	return api.DevEnvironment{
		Id: e.ID, OwnerId: e.OwnerID, Name: e.Name, SourceKind: api.DevEnvironmentSourceKind(e.SourceKind),
		WatchedTag: e.WatchedTag, AcceptedDefaultVersionId: e.DefaultVersionID, Definition: definition,
	}, nil
}

func versionToAPI(v dbq.EnvironmentVersion) (api.EnvironmentVersion, error) {
	definition, err := decodeDefinition(v.Definition)
	if err != nil {
		return api.EnvironmentVersion{}, err
	}
	capability, err := decodeStringMap(v.CapabilityProfile)
	if err != nil {
		return api.EnvironmentVersion{}, err
	}
	result, err := decodeStringMap(v.ValidationResult)
	if err != nil {
		return api.EnvironmentVersion{}, err
	}
	var packages *api.PackageDeclaration
	if isSet(v.PackageDeclaration) {
		packages = new(api.PackageDeclaration)
		if err := json.Unmarshal(v.PackageDeclaration, packages); err != nil {
			return api.EnvironmentVersion{}, err
		}
	}
	var arch *api.EnvironmentVersionArchitecture
	if v.Architecture != nil {
		a := api.EnvironmentVersionArchitecture(*v.Architecture)
		arch = &a
	}
	return api.EnvironmentVersion{
		Id: v.ID, EnvironmentId: v.EnvironmentID, Registry: v.Registry, Repository: v.Repository,
		IndexDigest: v.IndexDigest, PlatformManifestDigest: v.PlatformManifestDigest, ConfigDigest: v.ConfigDigest,
		DeclaredUser: v.DeclaredUser, Architecture: arch, CapabilityProfile: capability,
		ValidationStatus: api.EnvironmentVersionValidationStatus(v.ValidationStatus), ValidationResult: result,
		ValidatorVersion: v.ValidatorVersion, ProvenanceKind: api.EnvironmentVersionProvenanceKind(v.ProvenanceKind),
		BaseProvenance: api.EnvironmentVersionBaseProvenance(v.BaseProvenance), ParentVersionId: v.ParentVersionID,
		PackageDeclaration: packages, ApprovalReference: v.ApprovalReference, Definition: definition,
	}, nil
}

func isSet(raw json.RawMessage) bool { return len(raw) > 0 && string(raw) != "null" }

func decodeDefinition(raw json.RawMessage) (*api.DefinitionReference, error) {
	if !isSet(raw) {
		return nil, nil
	}
	var d api.DefinitionReference
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func decodeStringMap(raw json.RawMessage) (map[string]string, error) {
	m := map[string]string{}
	if !isSet(raw) {
		return m, nil
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}
