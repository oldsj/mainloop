package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/projectcontext"
)

// projectTools read what a project's owner tells every agent working on it
// (docs/specs/project-context.md). A task brief already carries the instructions at the version
// its attempt was pinned to; project_context reads the current ones.
func (s *Server) projectTools() []Tool {
	return []Tool{{
		Name: agentauth.ToolProjectContext,
		Description: "Read the project owner's current standing instructions for the project: instructions and version. " +
			"A task brief carries them at the version current when the task started; read this when you need " +
			"the current text. The main session may read any of its owner's projects; other sessions only their own.",
		InputSchema: objectSchema(map[string]any{"project_id": stringSchema(1, 100)}, "project_id"),
		Handle:      s.projectContext,
	}}
}

func (s *Server) projectContext(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	var in struct {
		ProjectID string `json:"project_id"`
	}
	if err := decode(raw, &in); err != nil {
		return Result{}, err
	}
	var err error
	if in.ProjectID, err = text(in.ProjectID, 1, 100); err != nil {
		return Result{}, err
	}
	if a.Role != dbstate.RoleMain {
		if err := projectAuthority(a, in.ProjectID); err != nil {
			return Result{}, err
		}
	}
	// The project must be the actor's owner's, whatever the role.
	if _, err := s.Queries.GetOwnedProject(ctx, dbq.GetOwnedProjectParams{ID: in.ProjectID, UserID: a.UserID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Result{}, failure("not_found", "no such project")
		}
		return Result{}, err
	}
	cur, err := projectcontext.Current(ctx, s.Queries, in.ProjectID)
	if err != nil {
		return Result{}, err
	}
	m := map[string]any{
		"project_id": in.ProjectID, "version": cur.Version, "instructions": cur.Text,
		"max_bytes": projectcontext.MaxInstructionsBytes,
	}
	line := "This project has no owner instructions."
	if cur.UpdatedAt != nil {
		m["updated_at"] = cur.UpdatedAt.UTC().Format(time.RFC3339)
	}
	if cur.Text != "" {
		line = fmt.Sprintf("Project instructions (owner, v%d):\n%s", cur.Version, cur.Text)
	}
	m["text"] = line
	return Result{Text: line, Structured: m}, nil
}
