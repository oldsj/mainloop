package ci

// Verification: where a project's heavy checks run. The owner picks a mode per project, or leaves it
// on auto, which follows a cached detection of the project's CI: hybrid when CI reported checks on
// the default branch's tip, local otherwise. The detection is read through the [Provider], so it
// works for any CI that reports to the forge, and it is refreshed when it is a day old.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// DetectionTTL is how long a CI detection is trusted before a read refreshes it.
const DetectionTTL = 24 * time.Hour

// Detection bounds: the check sources it records, how many recently closed pull requests it lists,
// and how many merged heads it reads checks on when the default branch shows none.
const (
	maxSources     = 10
	mergedLookback = 10
	mergedProbes   = 3
)

// ErrInvalidMode is a verification mode that is not one of the four.
var ErrInvalidMode = errors.New("ci: unknown verification mode")

// Detection is what Mainloop found of a project's CI. It is stored as JSON, so later reads (whether
// a workflow deploys or publishes on push, what CI costs) add fields here without a migration.
type Detection struct {
	// Usable is true when at least one check reported on the default branch's tip or, failing
	// that, on the head of one of the last merged pull requests into it.
	Usable bool `json:"usable"`
	// Branch is the default branch. SHA is the commit whose checks were counted: the branch's tip,
	// or the head of pull request PR when CI was found there (CI that runs only on pull requests).
	Branch string `json:"branch"`
	SHA    string `json:"sha,omitempty"`
	PR     int    `json:"pr_number,omitempty"`
	// Checks counts the checks on that commit, and Sources names what reported them: the check
	// run's app, or the commit status's context. At most ten.
	Checks  int      `json:"checks"`
	Sources []string `json:"sources,omitempty"`
	// CheckedAt is when the detection ran.
	CheckedAt time.Time `json:"-"`
}

// Source says where an effective mode came from.
type Source string

// Sources of the effective mode.
const (
	// SourceOwner: the owner chose the mode.
	SourceOwner Source = "owner"
	// SourceDetected: auto, from a detection.
	SourceDetected Source = "detected"
	// SourceDefault: auto, with no detection to go on (none yet, or it could not be read).
	SourceDefault Source = "default"
)

// Verification is a project's setting, what it resolves to, and why.
type Verification struct {
	Mode      dbstate.VerificationMode
	Effective dbstate.VerificationMode
	Source    Source
	Detection *Detection
	// DetectError says why a refresh failed, when it did; the last good detection, if any, still
	// stands.
	DetectError string
}

// ValidMode reports whether m is one of the four modes.
func ValidMode(m dbstate.VerificationMode) bool {
	return slices.Contains([]dbstate.VerificationMode{dbstate.VerifyAuto, dbstate.VerifyCI, dbstate.VerifyHybrid, dbstate.VerifyLocal}, m)
}

// Resolve is the effective mode of a setting and a detection.
func Resolve(mode dbstate.VerificationMode, d *Detection) (dbstate.VerificationMode, Source) {
	switch {
	case mode != dbstate.VerifyAuto:
		return mode, SourceOwner
	case d == nil:
		return dbstate.VerifyLocal, SourceDefault
	case d.Usable:
		return dbstate.VerifyHybrid, SourceDetected
	}
	return dbstate.VerifyLocal, SourceDetected
}

// Verification reads a project's verification setting. It detects the project's CI first when
// there is no detection yet, when the last one is older than [DetectionTTL], or when refresh is
// set. A failed detection is reported in DetectError and leaves the last good one in place.
func (s *Service) Verification(ctx context.Context, ownerID, projectID string, refresh bool) (Verification, error) {
	row, err := s.verificationRow(ctx, ownerID, projectID)
	if err != nil {
		return Verification{}, err
	}
	d, err := decodeDetection(row)
	if err != nil {
		return Verification{}, err
	}
	var detectErr string
	if refresh || d == nil || s.now().Sub(d.CheckedAt) >= DetectionTTL {
		fresh, err := s.detect(ctx, ownerID, row)
		var r *Refusal
		switch {
		case errors.As(err, &r):
			detectErr = r.Message
		case err != nil:
			// Storing failed (or the caller's credential was revoked under the store's guard).
			return Verification{}, err
		default:
			d = &fresh
		}
	}
	return s.view(dbstate.VerificationMode(row.VerificationMode), d, detectErr), nil
}

// SetVerificationMode records the owner's choice. It does not detect: auto resolves from the stored
// detection, and the next read refreshes a stale one.
func (s *Service) SetVerificationMode(ctx context.Context, ownerID, projectID string, mode dbstate.VerificationMode) (Verification, error) {
	if !ValidMode(mode) {
		return Verification{}, ErrInvalidMode
	}
	n, err := dbq.New(s.Pool).SetProjectVerificationMode(ctx, dbq.SetProjectVerificationModeParams{
		ID: projectID, UserID: ownerID, VerificationMode: string(mode),
	})
	if err != nil {
		return Verification{}, err
	}
	if n == 0 {
		return Verification{}, ErrProjectNotFound
	}
	row, err := s.verificationRow(ctx, ownerID, projectID)
	if err != nil {
		return Verification{}, err
	}
	d, err := decodeDetection(row)
	if err != nil {
		return Verification{}, err
	}
	return s.view(dbstate.VerificationMode(row.VerificationMode), d, ""), nil
}

func (s *Service) view(mode dbstate.VerificationMode, d *Detection, detectErr string) Verification {
	eff, src := Resolve(mode, d)
	return Verification{Mode: mode, Effective: eff, Source: src, Detection: d, DetectError: detectErr}
}

func (s *Service) verificationRow(ctx context.Context, ownerID, projectID string) (dbq.GetProjectVerificationRow, error) {
	row, err := dbq.New(s.Pool).GetProjectVerification(ctx, dbq.GetProjectVerificationParams{ID: projectID, UserID: ownerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, ErrProjectNotFound
	}
	return row, err
}

func decodeDetection(row dbq.GetProjectVerificationRow) (*Detection, error) {
	if len(row.CiDetection) == 0 || row.CiDetectedAt == nil {
		return nil, nil
	}
	var d Detection
	if err := json.Unmarshal(row.CiDetection, &d); err != nil {
		return nil, fmt.Errorf("ci: stored detection: %w", err)
	}
	d.CheckedAt = *row.CiDetectedAt
	return &d, nil
}

// detect reads the checks on the default branch's tip and stores what it found. No transaction is
// open across the reads; two concurrent detections both write a current answer.
func (s *Service) detect(ctx context.Context, ownerID string, row dbq.GetProjectVerificationRow) (Detection, error) {
	owner, name, ok := strings.Cut(row.FullName, "/")
	if !ok || owner == "" || name == "" {
		return Detection{}, &Refusal{"repository_mismatch", "the project has no usable repository name"}
	}
	repo := Repo{Owner: owner, Name: name}
	d := Detection{Branch: row.DefaultBranch, CheckedAt: s.now()}
	sha, err := s.Provider.BranchHead(ctx, repo, row.DefaultBranch)
	switch {
	case errors.Is(err, ErrNotFound):
		// An empty repository, or a default branch that moved: nothing reports there yet.
	case err != nil:
		return Detection{}, unavailable(err)
	default:
		d.SHA = sha
		if err := s.countChecks(ctx, repo, &d); err != nil {
			return Detection{}, err
		}
	}
	if !d.Usable {
		// CI that runs only on pull requests reports nothing on the default branch; its checks
		// stay on the heads of the pull requests merged into it.
		heads, err := s.Provider.MergedHeads(ctx, repo, row.DefaultBranch, mergedLookback)
		if err != nil {
			return Detection{}, unavailable(err)
		}
		for _, h := range heads[:min(len(heads), mergedProbes)] {
			probe := Detection{Branch: d.Branch, SHA: h.SHA, PR: h.PR, CheckedAt: d.CheckedAt}
			if err := s.countChecks(ctx, repo, &probe); err != nil {
				return Detection{}, err
			}
			if probe.Usable {
				d = probe
				break
			}
		}
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return Detection{}, err
	}
	at := d.CheckedAt
	// In a transaction, so an agent's call re-checks its credential before the write.
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		_, err := dbq.New(tx).SetProjectCIDetection(ctx, dbq.SetProjectCIDetectionParams{
			ID: row.ID, UserID: ownerID, CiDetection: raw, CiDetectedAt: &at,
		})
		return err
	})
	if err != nil {
		return Detection{}, fmt.Errorf("store detection: %w", err)
	}
	return d, nil
}

// Instructions is what an agent does under an effective mode, in a sentence or two.
func Instructions(effective dbstate.VerificationMode) string {
	switch effective {
	case dbstate.VerifyCI:
		return "CI runs the project's checks. In the workspace, only build and lint what you changed. " +
			"Push once, call watch_checks, end your turn, and fix from the failure detail it sends."
	case dbstate.VerifyHybrid:
		return "CI runs the full suite. In the workspace, build, lint and run the tests of what you changed. " +
			"Push once those pass, call watch_checks, end your turn, and fix from the failure detail it sends."
	}
	return "No usable CI: run the project's full checks in the workspace before you report done. " +
		"If the project should have CI, propose a starter workflow in its own pull request."
}

// countChecks reads the checks on d.SHA into d.
func (s *Service) countChecks(ctx context.Context, repo Repo, d *Detection) error {
	checks, err := s.Provider.Checks(ctx, repo, d.SHA)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return unavailable(err)
	}
	d.Checks, d.Usable = len(checks), len(checks) > 0
	for _, c := range checks {
		src := c.App
		if src == "" || c.Kind == KindCommitStatus {
			src = c.Name
		}
		if src != "" && !slices.Contains(d.Sources, src) && len(d.Sources) < maxSources {
			d.Sources = append(d.Sources, src)
		}
	}
	slices.Sort(d.Sources)
	return nil
}
