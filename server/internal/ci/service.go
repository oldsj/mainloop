package ci

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/github"
)

// Bounds on what one status carries, so a noisy CI cannot flood an agent's context.
const (
	// MaxDetailed is how many failed checks get their failure detail read; the rest are listed.
	MaxDetailed = 5
	// maxSummary and maxText cap a check's own output, in runes.
	maxSummary = 2000
	maxText    = 4000
	// maxAnnotationsShown caps the annotations kept per failure, and maxMessage each message.
	maxAnnotationsShown = 20
	maxMessage          = 500
)

// ErrProjectNotFound means the project is not the owner's.
var ErrProjectNotFound = errors.New("ci: project not found")

// Refusal is a request Mainloop refused. Code is stable.
type Refusal struct{ Code, Message string }

func (r *Refusal) Error() string { return r.Code + ": " + r.Message }

// Ref names the commit to read: a pull request's current head, or a commit by SHA. Exactly one is
// set.
type Ref struct {
	PR  int
	SHA string
}

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Valid reports whether exactly one of PR and SHA is set and well formed.
func (r Ref) Valid() bool {
	if r.PR > 0 {
		return r.SHA == ""
	}
	return r.PR == 0 && shaPattern.MatchString(r.SHA)
}

// Status is the verdict over one commit's checks, with why the failed ones failed.
type Status struct {
	State  State
	SHA    string
	PR     int
	Counts Counts
	Checks []Check
	// Failures holds the detail of the first [MaxDetailed] failed checks; FailuresOmitted counts
	// the failed checks beyond them, which are still listed in Checks.
	Failures        []Failure
	FailuresOmitted int
}

// Service serves the check tools over a project's bound repository.
type Service struct {
	Pool     *pgxpool.Pool
	Provider Provider
}

// NewService builds the service over the GitHub App.
func NewService(pool *pgxpool.Pool, api GitHubAPI) *Service {
	return &Service{Pool: pool, Provider: GitHub{API: api}}
}

// project resolves the owner's project to the repository it names.
func (s *Service) project(ctx context.Context, ownerID, projectID string) (Repo, error) {
	p, err := dbq.New(s.Pool).GetProjectAuthority(ctx, dbq.GetProjectAuthorityParams{ID: projectID, UserID: ownerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Repo{}, ErrProjectNotFound
	}
	if err != nil {
		return Repo{}, err
	}
	owner, name, ok := strings.Cut(p.FullName, "/")
	if !ok || owner == "" || name == "" {
		return Repo{}, &Refusal{"repository_mismatch", "the project has no usable repository name"}
	}
	return Repo{Owner: owner, Name: name}, nil
}

func unavailable(err error) error {
	text := "unreachable"
	if st, ok := github.StatusOf(err); ok {
		text = fmt.Sprintf("status %d", st)
	}
	return &Refusal{"ci_unavailable", "CI could not be read: " + text}
}

// Status reads the checks at ref, in the owner's project, and the detail of the failed ones. A
// pull request is resolved to its current head, so the answer reflects the latest push.
func (s *Service) Status(ctx context.Context, ownerID, projectID string, ref Ref) (Status, error) {
	if !ref.Valid() {
		return Status{}, &Refusal{"invalid_ref", "give exactly one of pr_number or a 40-character commit_sha"}
	}
	repo, err := s.project(ctx, ownerID, projectID)
	if err != nil {
		return Status{}, err
	}
	sha := ref.SHA
	if ref.PR > 0 {
		sha, err = s.Provider.HeadSHA(ctx, repo, ref.PR)
		switch {
		case errors.Is(err, ErrNotFound):
			return Status{}, &Refusal{"not_found", "no such pull request"}
		case err != nil:
			return Status{}, unavailable(err)
		}
	}
	checks, err := s.Provider.Checks(ctx, repo, sha)
	switch {
	case errors.Is(err, ErrNotFound):
		return Status{}, &Refusal{"not_found", "no such commit"}
	case err != nil:
		return Status{}, unavailable(err)
	}
	st := Status{SHA: sha, PR: ref.PR}
	st.State, st.Counts = Judge(checks)
	for _, c := range checks {
		if c.Result != Failed {
			continue
		}
		if len(st.Failures) == MaxDetailed {
			st.FailuresOmitted++
			continue
		}
		f, err := s.Provider.Failure(ctx, repo, c)
		if err != nil {
			return Status{}, unavailable(err)
		}
		st.Failures = append(st.Failures, bound(f))
	}
	// Listings carry the short summary only; the long text went into the failures above.
	for i := range checks {
		checks[i].Summary = clip(checks[i].Summary, maxSummary)
		checks[i].Text = ""
	}
	st.Checks = checks
	return st, nil
}

// bound caps a failure's text so one noisy check cannot flood the reader.
func bound(f Failure) Failure {
	f.Summary = clip(f.Summary, maxSummary)
	f.Text = clip(f.Text, maxText)
	f.LogTail = tail(f.LogTail, maxText)
	if len(f.Annotations) > maxAnnotationsShown {
		f.Annotations = f.Annotations[:maxAnnotationsShown]
	}
	for i := range f.Annotations {
		f.Annotations[i].Message = clip(f.Annotations[i].Message, maxMessage)
		f.Annotations[i].Title = clip(f.Annotations[i].Title, maxMessage)
	}
	return f
}

// clip keeps the first n runes of s.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// tail keeps the last n runes of s: the end of a log is where it failed.
func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}
