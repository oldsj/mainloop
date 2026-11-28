package dotfiles

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/devloop"
	"github.com/oldsj/mainloop/server/internal/github"
)

// Status of one session's dotfiles, as recorded and shown.
const (
	// StatusReady: the export exists and the session is given its bundle.
	StatusReady = "ready"
	// StatusNone: no source applies to the session.
	StatusNone = "none"
	// StatusUnavailable: the source could not be read; a later session may read it.
	StatusUnavailable = "unavailable"
	// StatusRefused: the commit's directory cannot be exported, and never will be.
	StatusRefused = "refused"
)

// Reasons a source is unavailable.
const (
	ReasonAppNotInstalled       = "app_not_installed"
	ReasonRefNotFound           = "ref_not_found"
	ReasonRepositoryUnavailable = "repository_unavailable"
)

// ExportFormat is the format of the exports this code makes, part of a stored export's key. Bump it
// whenever what an export keeps or how it is bundled changes, so an export stored under an older
// format is read again rather than reused (a test holds a bump to that). 1: the first export; 2:
// claude/settings.json is kept (#564).
const ExportFormat = 2

// RefResolver resolves a branch, tag or commit to a full commit SHA. [*github.App] satisfies it.
type RefResolver interface {
	CommitSHA(ctx context.Context, repo github.Repo, ref string) (string, error)
}

// Result is one export's outcome. Commit is set once the ref resolved. Bundle and Digest are set
// when Status is ready; Reason when it is not; Findings when a secret refused it.
type Result struct {
	Commit   string
	Status   string
	Reason   string
	Digest   string
	Bundle   []byte
	Findings []Finding
}

// Exports reads dotfiles sources from GitHub server-side and keeps each commit's export. Agents
// never get access to the repository: only the bundle of the allowed files leaves Mainloop.
//
// The reconcile rule holds: GitHub is read with no transaction open, and what is learned is then
// written in one short transaction. Two sessions exporting the same commit at once both write the
// same rows; the second write changes nothing.
type Exports struct {
	Pool   *pgxpool.Pool
	Refs   RefResolver
	Seeder devloop.GitSeeder
	Limits Limits
	// ReadTimeout bounds each GitHub read: resolving the ref, and fetching a commit not exported
	// yet. A pin runs before a create, under a reconciler lease, so it must stay well inside one.
	// Zero means 10 seconds.
	ReadTimeout time.Duration
	Log         *slog.Logger
	// OwnerExported, when set, is told of each ready pin of an owner's own source: the owner, the
	// commit and that commit's AGENTS.md (nil when it has none). Owner memory checks its entries
	// against it (docs/specs/owner-memory.md#moving-an-entry-to-dotfiles).
	OwnerExported func(ctx context.Context, ownerID, commit string, agentsMD []byte) error

	heads headCache
}

// Resolve returns the commit src's ref names now. A ref that cannot be resolved is an
// unavailable Result, not an error: a session runs without dotfiles rather than not at all.
func (e *Exports) Resolve(ctx context.Context, src Source) (string, *Result) {
	repo, err := github.ParseRepo(src.Repository)
	if err != nil {
		return "", &Result{Status: StatusUnavailable, Reason: ReasonRepositoryUnavailable}
	}
	if e.Refs == nil {
		return "", &Result{Status: StatusUnavailable, Reason: ReasonRepositoryUnavailable}
	}
	ref := src.Ref
	if ref == "" {
		ref = "HEAD"
	}
	rctx, cancel := context.WithTimeout(ctx, e.readTimeout())
	defer cancel()
	sha, err := e.Refs.CommitSHA(rctx, repo, ref)
	switch {
	case errors.Is(err, github.ErrNotInstalled):
		return "", &Result{Status: StatusUnavailable, Reason: ReasonAppNotInstalled}
	case errors.Is(err, github.ErrNotFound), errors.Is(err, github.ErrBranchName):
		return "", &Result{Status: StatusUnavailable, Reason: ReasonRefNotFound}
	case err != nil:
		e.log().WarnContext(ctx, "dotfiles: resolve failed", "repository", src.Repository, "error_type", fmt.Sprintf("%T", err))
		return "", &Result{Status: StatusUnavailable, Reason: ReasonRepositoryUnavailable}
	}
	return sha, nil
}

// Export returns src's directory at commit: from the stored export of [ExportFormat] when there is
// one, else read from GitHub, checked and stored. A refusal is stored too, since the same commit always gives the
// same answer; an unavailable read is not.
func (e *Exports) Export(ctx context.Context, src Source, commit string) (Result, error) {
	key := exportKey(src, commit)
	if r, ok, err := e.stored(ctx, key); err != nil || ok {
		return r, err
	}
	r := e.read(ctx, src, commit)
	if r.Status == StatusUnavailable {
		return r, nil
	}
	if err := e.store(ctx, key, r); err != nil {
		return Result{}, err
	}
	return r, nil
}

// read fetches the commit and builds its export. It writes nothing.
func (e *Exports) read(ctx context.Context, src Source, commit string) Result {
	repo, err := github.ParseRepo(src.Repository)
	if err != nil || e.Seeder == nil {
		return Result{Commit: commit, Status: StatusUnavailable, Reason: ReasonRepositoryUnavailable}
	}
	rctx, cancel := context.WithTimeout(ctx, e.readTimeout())
	defer cancel()
	archive, err := devloop.FetchArchive(rctx, e.Seeder, repo, commit, nil)
	if err != nil {
		e.log().WarnContext(ctx, "dotfiles: fetch failed", "repository", src.Repository, "commit", commit, "error_type", fmt.Sprintf("%T", err))
		return Result{Commit: commit, Status: StatusUnavailable, Reason: ReasonRepositoryUnavailable}
	}
	return Build(archive, src.Path, e.limits(), commit)
}

// Build turns a `git archive` tar of commit into the export of dir: select, scan, bundle.
func Build(archive []byte, dir string, lim Limits, commit string) Result {
	files, err := Select(bytes.NewReader(archive), dir, lim)
	var refusal *Refusal
	if errors.As(err, &refusal) {
		return Result{Commit: commit, Status: StatusRefused, Reason: refusal.Reason}
	}
	if err != nil {
		return Result{Commit: commit, Status: StatusUnavailable, Reason: ReasonRepositoryUnavailable}
	}
	if findings := Scan(files); len(findings) > 0 {
		return Result{Commit: commit, Status: StatusRefused, Reason: ReasonSecretFound, Findings: findings}
	}
	bundle, digest, err := Bundle(files)
	if err != nil {
		return Result{Commit: commit, Status: StatusUnavailable, Reason: ReasonRepositoryUnavailable}
	}
	return Result{Commit: commit, Status: StatusReady, Digest: digest, Bundle: bundle}
}

// BundleContent returns a stored bundle by its digest.
func (e *Exports) BundleContent(ctx context.Context, digest string) ([]byte, bool, error) {
	content, err := dbq.New(e.Pool).GetDotfilesBundle(ctx, digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return content, true, nil
}

type key struct {
	repository, commit, path string
	format                   int32
}

// exportKey compares repositories case-insensitively, as GitHub does.
func exportKey(src Source, commit string) key {
	return key{repository: strings.ToLower(src.Repository), commit: commit, path: src.Path, format: ExportFormat}
}

func (e *Exports) stored(ctx context.Context, k key) (Result, bool, error) {
	row, err := dbq.New(e.Pool).GetDotfilesExport(ctx, dbq.GetDotfilesExportParams{Repository: k.repository, Commit: k.commit, Path: k.path, Format: k.format})
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	r := Result{Commit: row.Commit, Status: row.Status, Reason: row.Reason}
	if row.Digest != nil {
		r.Digest = *row.Digest
	}
	if len(row.Findings) > 0 {
		if err := json.Unmarshal(row.Findings, &r.Findings); err != nil {
			return Result{}, false, err
		}
	}
	return r, true, nil
}

func (e *Exports) store(ctx context.Context, k key, r Result) error {
	findings, err := json.Marshal(r.Findings)
	if err != nil {
		return err
	}
	if r.Findings == nil {
		findings = []byte("[]")
	}
	return db.InTx(ctx, e.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		var digest *string
		if r.Status == StatusReady {
			digest = &r.Digest
			if err := q.InsertDotfilesBundle(ctx, dbq.InsertDotfilesBundleParams{Digest: r.Digest, Content: r.Bundle}); err != nil {
				return err
			}
		}
		return q.InsertDotfilesExport(ctx, dbq.InsertDotfilesExportParams{
			Repository: k.repository, Commit: k.commit, Path: k.path, Format: k.format,
			Status: r.Status, Reason: r.Reason, Digest: digest, Findings: findings,
		})
	})
}

func (e *Exports) limits() Limits {
	if e.Limits == (Limits{}) {
		return DefaultLimits
	}
	return e.Limits
}

func (e *Exports) readTimeout() time.Duration {
	if e.ReadTimeout <= 0 {
		return 10 * time.Second
	}
	return e.ReadTimeout
}

func (e *Exports) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}
