// Package projectcontext holds what a project's owner tells every agent working on it: the
// project's instructions, owner-authored and versioned (docs/specs/project-context.md). Repository
// facts belong in the repository's own AGENTS.md; this is the owner's standing direction that is
// not for the repository, or changes faster than a pull request.
package projectcontext

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// MaxInstructionsBytes bounds a project's instructions. They go into every task brief of the
// project next to a brief of up to 16 KiB, so they stay at half that. The migration holds the same
// bound.
const MaxInstructionsBytes = 8192

// maxVersionsListed is how many versions Versions returns.
const maxVersionsListed = 50

// Typed failures the API and tool layers map to their own answers.
var (
	ErrNotFound        = errors.New("projectcontext: project not found")
	ErrVersionConflict = errors.New("projectcontext: instructions version changed")
	ErrTooLarge        = errors.New("projectcontext: instructions are too large")
	ErrInvalidText     = errors.New("projectcontext: instructions are not valid UTF-8 text")
)

// Instructions is one version of a project's instructions. Version 0 with empty text means none
// were ever set; UpdatedAt is nil then.
type Instructions struct {
	ProjectID string
	Version   int
	Text      string
	UpdatedAt *time.Time
}

// Service reads and writes a project's instructions for its owner.
type Service struct {
	Pool    *pgxpool.Pool
	OwnerID string
}

// Instructions reads the project's current instructions.
func (s *Service) Instructions(ctx context.Context, projectID string) (Instructions, error) {
	q := dbq.New(s.Pool)
	if _, err := q.GetOwnedProject(ctx, dbq.GetOwnedProjectParams{ID: projectID, UserID: s.OwnerID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Instructions{}, ErrNotFound
		}
		return Instructions{}, fmt.Errorf("get project: %w", err)
	}
	return Current(ctx, q, projectID)
}

// SetInstructions replaces the project's instructions if expected is still the current version.
// Each change is a new version, written under the project row lock; writing the current text again
// changes nothing and keeps the version. Empty text (after trimming) clears the instructions as a
// new version.
func (s *Service) SetInstructions(ctx context.Context, projectID, text string, expected int) (Instructions, error) {
	text = strings.TrimSpace(text)
	if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return Instructions{}, ErrInvalidText
	}
	if len(text) > MaxInstructionsBytes {
		return Instructions{}, ErrTooLarge
	}
	var out Instructions
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if _, err := q.LockOwnedProject(ctx, dbq.LockOwnedProjectParams{ID: projectID, UserID: s.OwnerID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		cur, err := Current(ctx, q, projectID)
		if err != nil {
			return err
		}
		if cur.Version != expected {
			return ErrVersionConflict
		}
		if cur.Text == text {
			out = cur
			return nil
		}
		row, err := q.InsertProjectInstructionVersion(ctx, dbq.InsertProjectInstructionVersionParams{
			ProjectID: projectID, Version: int32(cur.Version + 1), Text: text, //nolint:gosec // versions stay far below 2^31
		})
		if err != nil {
			return err
		}
		out = fromRow(row)
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrVersionConflict) {
			return Instructions{}, err
		}
		return Instructions{}, fmt.Errorf("set instructions: %w", err)
	}
	return out, nil
}

// Versions lists the project's instruction versions, newest first.
func (s *Service) Versions(ctx context.Context, projectID string) ([]Instructions, error) {
	q := dbq.New(s.Pool)
	if _, err := q.GetOwnedProject(ctx, dbq.GetOwnedProjectParams{ID: projectID, UserID: s.OwnerID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get project: %w", err)
	}
	rows, err := q.ListProjectInstructionVersions(ctx, dbq.ListProjectInstructionVersionsParams{ProjectID: projectID, Limit: maxVersionsListed})
	if err != nil {
		return nil, fmt.Errorf("list instruction versions: %w", err)
	}
	out := make([]Instructions, len(rows))
	for i, r := range rows {
		out[i] = fromRow(r)
	}
	return out, nil
}

// Current reads the project's current instructions without an ownership check; callers have
// established the project already.
func Current(ctx context.Context, q *dbq.Queries, projectID string) (Instructions, error) {
	row, err := q.GetCurrentProjectInstructions(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Instructions{ProjectID: projectID}, nil
	}
	if err != nil {
		return Instructions{}, fmt.Errorf("get instructions: %w", err)
	}
	return fromRow(row), nil
}

// ForAttempt returns the instructions an attempt's brief carries. The first call pins the
// project's current version on the attempt; later calls, such as a resend of the brief, read that
// pinned version, so every send of one attempt's brief carries the same text.
func ForAttempt(ctx context.Context, q *dbq.Queries, attemptID, projectID string) (Instructions, error) {
	cur, err := Current(ctx, q, projectID)
	if err != nil {
		return Instructions{}, err
	}
	pinned, err := q.PinAttemptInstructionsVersion(ctx, dbq.PinAttemptInstructionsVersionParams{
		ID: attemptID, Version: int32(cur.Version), //nolint:gosec // versions stay far below 2^31
	})
	if err != nil {
		return Instructions{}, fmt.Errorf("pin instructions version: %w", err)
	}
	if pinned == nil || int(*pinned) == cur.Version {
		return cur, nil
	}
	if *pinned == 0 {
		return Instructions{ProjectID: projectID}, nil
	}
	row, err := q.GetProjectInstructionVersion(ctx, dbq.GetProjectInstructionVersionParams{ProjectID: projectID, Version: *pinned})
	if err != nil {
		return Instructions{}, fmt.Errorf("get pinned instructions: %w", err)
	}
	return fromRow(row), nil
}

// Section renders the instructions as the brief section that carries them, or "" when there are
// none.
func Section(in Instructions) string {
	if in.Version == 0 || in.Text == "" {
		return ""
	}
	return fmt.Sprintf("## Project instructions (owner, v%d)\n"+
		"The project owner's standing instructions for every task in this project. Follow them unless the "+
		"assigned work explicitly overrides one, and say so in your report when it does.\n\n%s\n", in.Version, in.Text)
}

func fromRow(r dbq.ProjectInstructionVersion) Instructions {
	at := r.CreatedAt
	return Instructions{ProjectID: r.ProjectID, Version: int(r.Version), Text: r.Text, UpdatedAt: &at}
}
