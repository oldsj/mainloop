// Package onboarding turns "make this repository a Mainloop project" into one owner decision
// (docs/specs/projects.md#onboarding-proposals). Main or a project supervisor proposes; Mainloop
// checks what it can (the GitHub App's installation, the environment image, whether the repository
// is already a project), stores the proposal as a durable intent row and puts one approval card in
// Needs you, in the same transaction.
package onboarding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/attention"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/environments"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/projects"
)

// Proposal states. A proposal waits in proposed until the owner answers its card; approved ones
// are applied through the project APIs and end applied or failed.
const (
	StateProposed = "proposed"
	StateApproved = "approved"
	StateRejected = "rejected"
	StateApplied  = "applied"
	StateFailed   = "failed"
)

// The card's two answers.
const (
	AnswerApprove = "Approve"
	AnswerReject  = "Reject"
)

// GitHub App installation, as found when proposing.
const (
	AppInstalled    = "installed"
	AppNotInstalled = "not_installed"
	AppUnknown      = "unknown"
)

// Bounds of a proposal.
const (
	MaxRequestIDLen = 100
	MaxWhyLen       = 1000
	MaxServices     = 8
	MaxServiceLen   = 100
	MaxSizeLen      = 40
	MaxImageLen     = 512
)

// VerificationModes are the project checks settings a proposal may name.
var VerificationModes = []string{"auto", "ci", "hybrid", "local"}

// Architectures an environment image may be checked for.
var Architectures = []string{"amd64", "arm64"}

// QueueIDPrefix starts the Needs-you queue item id of a proposal's card; the rest is the proposal id.
const QueueIDPrefix = "project_proposal_"

var (
	// ErrInvalid is a proposal that breaks the contract; the message says how.
	ErrInvalid = errors.New("onboarding: invalid proposal")
	// ErrConflict is a request id reused for a different proposal.
	ErrConflict = errors.New("onboarding: request_id was used for a different proposal")
)

// Spec is what the proposer asks for. Services and WorkspaceSize are recorded and shown to the
// owner, but nothing applies them: Mainloop has no declared services or workspace sizes yet.
type Spec struct {
	Repository              string                `json:"repository"`
	Why                     string                `json:"why"`
	EnvironmentImage        string                `json:"environment_image,omitempty"`
	EnvironmentArchitecture string                `json:"environment_architecture,omitempty"`
	VerificationMode        string                `json:"verification_mode,omitempty"`
	WorkspaceService        *api.WorkspaceService `json:"workspace_service,omitempty"`
	Services                []string              `json:"services,omitempty"`
	WorkspaceSize           string                `json:"workspace_size,omitempty"`
}

// Facts are what Mainloop found when the proposal was made.
type Facts struct {
	GitHubApp       string `json:"github_app"`
	GitHubAppDetail string `json:"github_app_detail,omitempty"`
	// ProjectID is set when the repository is already one of the owner's projects.
	ProjectID *string `json:"project_id,omitempty"`
	// EnvironmentDigest is the image's platform manifest digest, set when an image was proposed
	// and passed the static check.
	EnvironmentDigest string    `json:"environment_digest,omitempty"`
	CheckedAt         time.Time `json:"checked_at"`
}

// Proposer is who proposes, resolved by the server from the calling session.
type Proposer struct {
	Role      string
	SessionID *string
	TaskID    *string
}

// Proposal is a stored proposal.
type Proposal struct {
	ID          string
	RequestID   string
	Repository  string
	State       string
	Spec        Spec
	Facts       Facts
	ProjectID   *string
	QueueItemID string
	CreatedAt   time.Time
	// Attempts and LastError say how applying an approved proposal went; FinishedAt is set once it
	// was rejected, applied or failed.
	Attempts   int
	LastError  *string
	FinishedAt *time.Time
	// Replayed is set when the call returned a proposal made earlier: the same request id, or an
	// open proposal of the same repository.
	Replayed bool
}

// InstallChecker answers whether the GitHub App is installed on a repository.
type InstallChecker interface {
	Installed(ctx context.Context, repo github.Repo) (bool, error)
}

// ImageChecker statically validates an environment image, as environment registration does.
type ImageChecker interface {
	Validate(ctx context.Context, reference, architecture, environmentID, versionID string) (dbq.InsertEnvironmentVersionParams, error)
}

// Service makes proposals.
type Service struct {
	Pool    *pgxpool.Pool
	OwnerID string
	// GitHub checks the App's installation; nil reports it unknown.
	GitHub InstallChecker
	// Images checks a proposed environment image; nil refuses a proposal that names one.
	Images ImageChecker
	Log    *slog.Logger
	Now    func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Propose validates spec, returns the proposal already made under requestID or for the same
// repository, and otherwise checks the repository and image (with no transaction open), then
// stores the proposal and opens its card together.
func (s *Service) Propose(ctx context.Context, by Proposer, requestID string, spec Spec) (Proposal, error) {
	repo, err := normalize(&spec)
	if err != nil {
		return Proposal{}, err
	}
	if requestID = strings.TrimSpace(requestID); requestID == "" || utf8.RuneCountInString(requestID) > MaxRequestIDLen {
		return Proposal{}, fmt.Errorf("%w: request_id must be 1 to %d characters", ErrInvalid, MaxRequestIDLen)
	}
	q := dbq.New(s.Pool)
	if p, ok, err := s.existing(ctx, q, requestID, spec); ok || err != nil {
		return p, err
	}

	facts, err := s.check(ctx, q, repo, spec)
	if err != nil {
		return Proposal{}, err
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return Proposal{}, err
	}
	factsJSON, err := json.Marshal(facts)
	if err != nil {
		return Proposal{}, err
	}

	var row dbq.ProjectProposal
	inserted := false
	err = events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		tq := dbq.New(tx)
		r, err := tq.InsertProjectProposal(ctx, dbq.InsertProjectProposalParams{
			ID: uuid.NewString(), OwnerID: s.OwnerID, RequestID: requestID, Repository: spec.Repository,
			Spec: specJSON, Facts: factsJSON, ProposerRole: by.Role, ProposerSessionID: by.SessionID,
			ProposerTaskID: by.TaskID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // a concurrent proposal won; read it below
		}
		if err != nil {
			return nil, err
		}
		row, inserted = r, true
		return attention.Open(ctx, tx, s.OwnerID, card(r.ID, by, spec, facts))
	})
	if err != nil {
		return Proposal{}, err
	}
	if !inserted {
		if p, ok, err := s.existing(ctx, q, requestID, spec); ok || err != nil {
			return p, err
		}
		return Proposal{}, errors.New("onboarding: proposal insert conflicted but no proposal was found")
	}
	s.log().InfoContext(ctx, "project proposal opened", "proposal_id", row.ID, "repository", row.Repository,
		"github_app", facts.GitHubApp)
	return fromRow(row)
}

// existing returns the proposal this call repeats: the one made under requestID (if its spec is
// the same; a different spec is ErrConflict), else the repository's open one.
func (s *Service) existing(ctx context.Context, q *dbq.Queries, requestID string, spec Spec) (Proposal, bool, error) {
	row, err := q.GetProjectProposalByRequest(ctx, dbq.GetProjectProposalByRequestParams{OwnerID: s.OwnerID, RequestID: requestID})
	if err == nil {
		p, err := fromRow(row)
		if err != nil {
			return Proposal{}, true, err
		}
		if !reflect.DeepEqual(p.Spec, spec) {
			return Proposal{}, true, ErrConflict
		}
		p.Replayed = true
		return p, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Proposal{}, true, err
	}
	row, err = q.GetOpenProjectProposalForRepo(ctx, dbq.GetOpenProjectProposalForRepoParams{OwnerID: s.OwnerID, Repository: spec.Repository})
	if errors.Is(err, pgx.ErrNoRows) {
		return Proposal{}, false, nil
	}
	if err != nil {
		return Proposal{}, true, err
	}
	p, err := fromRow(row)
	p.Replayed = true
	return p, true, err
}

// check reads what the card needs. A GitHub failure is a fact, not an error; an image the static
// check refuses fails the proposal, so the proposer can fix it before the owner sees it.
func (s *Service) check(ctx context.Context, q *dbq.Queries, repo github.Repo, spec Spec) (Facts, error) {
	f := Facts{GitHubApp: AppUnknown, CheckedAt: s.now().UTC()}
	if s.GitHub == nil {
		f.GitHubAppDetail = "this Mainloop has no GitHub App configured"
	} else {
		ok, err := s.GitHub.Installed(ctx, repo)
		switch {
		case err != nil:
			f.GitHubAppDetail = "GitHub could not be asked: " + err.Error()
		case ok:
			f.GitHubApp = AppInstalled
		default:
			f.GitHubApp = AppNotInstalled
		}
	}
	id, err := q.GetOwnedProjectIDByRepo(ctx, dbq.GetOwnedProjectIDByRepoParams{UserID: s.OwnerID, Repository: spec.Repository})
	switch {
	case err == nil:
		f.ProjectID = &id
	case !errors.Is(err, pgx.ErrNoRows):
		return Facts{}, err
	}
	if spec.EnvironmentImage != "" {
		if s.Images == nil {
			return Facts{}, fmt.Errorf("%w: environment images cannot be checked here", ErrInvalid)
		}
		v, err := s.Images.Validate(ctx, spec.EnvironmentImage, spec.EnvironmentArchitecture, "proposal", "proposal")
		if err != nil {
			var re *environments.RegistryError
			if errors.As(err, &re) {
				return Facts{}, fmt.Errorf("%w: environment image: %s", ErrInvalid, re.Message)
			}
			return Facts{}, err
		}
		if v.PlatformManifestDigest != nil {
			f.EnvironmentDigest = *v.PlatformManifestDigest
		}
	}
	return f, nil
}

// normalize trims and checks spec in place and returns its repository.
func normalize(spec *Spec) (github.Repo, error) {
	repo, err := github.ParseRepo(spec.Repository)
	if err != nil {
		return github.Repo{}, fmt.Errorf("%w: repository must be owner/name or a GitHub URL", ErrInvalid)
	}
	spec.Repository = repo.FullName()
	spec.Why = strings.TrimSpace(spec.Why)
	if spec.Why == "" || utf8.RuneCountInString(spec.Why) > MaxWhyLen {
		return repo, fmt.Errorf("%w: why must be 1 to %d characters", ErrInvalid, MaxWhyLen)
	}
	spec.EnvironmentImage = strings.TrimSpace(spec.EnvironmentImage)
	spec.EnvironmentArchitecture = strings.TrimSpace(spec.EnvironmentArchitecture)
	switch {
	case spec.EnvironmentImage == "" && spec.EnvironmentArchitecture != "":
		return repo, fmt.Errorf("%w: environment_architecture needs environment_image", ErrInvalid)
	case len(spec.EnvironmentImage) > MaxImageLen:
		return repo, fmt.Errorf("%w: environment_image is longer than %d bytes", ErrInvalid, MaxImageLen)
	case spec.EnvironmentImage != "" && spec.EnvironmentArchitecture == "":
		spec.EnvironmentArchitecture = "amd64"
	}
	if spec.EnvironmentArchitecture != "" && !contains(Architectures, spec.EnvironmentArchitecture) {
		return repo, fmt.Errorf("%w: environment_architecture must be amd64 or arm64", ErrInvalid)
	}
	if spec.VerificationMode = strings.TrimSpace(spec.VerificationMode); spec.VerificationMode != "" && !contains(VerificationModes, spec.VerificationMode) {
		return repo, fmt.Errorf("%w: verification_mode must be one of %s", ErrInvalid, strings.Join(VerificationModes, ", "))
	}
	if ws := spec.WorkspaceService; ws != nil {
		if ws.Command == nil {
			ws.Command = []string{}
		}
		if ws.Ports == nil {
			ws.Ports = []api.WorkspacePort{}
		}
		if err := projects.ValidateWorkspaceService(*ws); err != nil {
			return repo, fmt.Errorf("%w: workspace_service: %s", ErrInvalid, err.Error())
		}
	}
	if len(spec.Services) > MaxServices {
		return repo, fmt.Errorf("%w: at most %d services", ErrInvalid, MaxServices)
	}
	for i, svc := range spec.Services {
		svc = strings.TrimSpace(svc)
		if !plainLine(svc, MaxServiceLen) {
			return repo, fmt.Errorf("%w: each service is one line of 1 to %d characters", ErrInvalid, MaxServiceLen)
		}
		spec.Services[i] = svc
	}
	if len(spec.Services) == 0 {
		spec.Services = nil
	}
	if spec.WorkspaceSize = strings.TrimSpace(spec.WorkspaceSize); spec.WorkspaceSize != "" && !plainLine(spec.WorkspaceSize, MaxSizeLen) {
		return repo, fmt.Errorf("%w: workspace_size is one line of 1 to %d characters", ErrInvalid, MaxSizeLen)
	}
	return repo, nil
}

func plainLine(s string, maxLen int) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= maxLen && !strings.ContainsFunc(s, unicode.IsControl)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func fromRow(r dbq.ProjectProposal) (Proposal, error) {
	p := Proposal{
		ID: r.ID, RequestID: r.RequestID, Repository: r.Repository, State: r.State, ProjectID: r.ProjectID,
		QueueItemID: QueueIDPrefix + r.ID, CreatedAt: r.CreatedAt, Attempts: int(r.Attempts),
		LastError: r.LastError, FinishedAt: r.FinishedAt,
	}
	if err := json.Unmarshal(r.Spec, &p.Spec); err != nil {
		return Proposal{}, fmt.Errorf("project proposal %s spec: %w", r.ID, err)
	}
	if err := json.Unmarshal(r.Facts, &p.Facts); err != nil {
		return Proposal{}, fmt.Errorf("project proposal %s facts: %w", r.ID, err)
	}
	return p, nil
}
