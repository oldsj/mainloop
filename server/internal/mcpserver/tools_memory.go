package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/projectcontext"
)

// memoryTools record and forget project memory (docs/specs/project-context.md). The server
// resolves where an entry came from; the agent only says what to remember.
func (s *Server) memoryTools() []Tool {
	return []Tool{{
		Name: agentauth.ToolRemember,
		Description: "Record a short lesson for every later agent on the project, such as an owner preference or a " +
			"verified fact that is not about the repository's code (that belongs in an AGENTS.md change). At most 500 " +
			"bytes, on one line. The main session's entries are active at once and go into every new task brief; " +
			"only record the owner's own words or facts you verified. Any other session's entry is a proposal the " +
			"owner accepts or rejects. Set replaces to the id of an active entry the new one supersedes. " +
			"Recording text that is already open answers that entry.",
		InputSchema: objectSchema(map[string]any{
			"project_id": stringSchema(1, 100),
			"text":       stringSchema(1, projectcontext.MaxMemoryEntryBytes),
			"replaces":   stringSchema(1, 100),
		}, "project_id", "text"),
		Handle: s.remember,
	}, {
		Name: agentauth.ToolForget,
		Description: "End a project memory entry. The main session ends an active entry that turned out wrong or " +
			"out of date; any other session withdraws a proposal it made itself.",
		InputSchema: objectSchema(map[string]any{
			"project_id": stringSchema(1, 100),
			"entry_id":   stringSchema(1, 100),
		}, "project_id", "entry_id"),
		Handle: s.forget,
	}}
}

func (s *Server) remember(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	var in struct {
		ProjectID string `json:"project_id"`
		Text      string `json:"text"`
		Replaces  string `json:"replaces"`
	}
	if err := decode(raw, &in); err != nil {
		return Result{}, err
	}
	svc, by, err := s.memoryActor(ctx, a, &in.ProjectID)
	if err != nil {
		return Result{}, err
	}
	e, err := svc.Remember(ctx, in.ProjectID, by, in.Text, in.Replaces)
	if err != nil {
		return Result{}, memoryFailure(err)
	}
	line := fmt.Sprintf("Memory %s is %s.", e.ID, e.State)
	switch e.State {
	case projectcontext.StateActive:
		line += " Every new task brief of the project carries it."
	case projectcontext.StateProposed:
		line += " The owner decides whether later agents get it."
	}
	return entryResult(e, line), nil
}

func (s *Server) forget(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	var in struct {
		ProjectID string `json:"project_id"`
		EntryID   string `json:"entry_id"`
	}
	if err := decode(raw, &in); err != nil {
		return Result{}, err
	}
	var err error
	if in.EntryID, err = text(in.EntryID, 1, 100); err != nil {
		return Result{}, err
	}
	svc, by, err := s.memoryActor(ctx, a, &in.ProjectID)
	if err != nil {
		return Result{}, err
	}
	e, err := svc.Forget(ctx, in.ProjectID, by, in.EntryID)
	if err != nil {
		return Result{}, memoryFailure(err)
	}
	return entryResult(e, fmt.Sprintf("Memory %s is %s.", e.ID, e.State)), nil
}

// memoryActor checks the actor's authority over the project and resolves who is recording: the
// author and the session, task, attempt and latest delivery it is acting in.
func (s *Server) memoryActor(ctx context.Context, a agentauth.Actor, projectID *string) (*projectcontext.Service, projectcontext.Recorder, error) {
	var none projectcontext.Recorder
	if s.Pool == nil {
		return nil, none, failure("unavailable", "project memory is not available")
	}
	var err error
	if *projectID, err = text(*projectID, 1, 100); err != nil {
		return nil, none, err
	}
	by := projectcontext.Recorder{Source: projectcontext.Source{SessionID: &a.BindingID}}
	switch a.Role {
	case dbstate.RoleMain:
		by.Author = projectcontext.AuthorMain
	case dbstate.RoleSupervisor, dbstate.RoleChild:
		by.Author = projectcontext.AuthorTask
	default:
		by.Author = projectcontext.AuthorAgent
	}
	if by.Author != projectcontext.AuthorMain {
		if err := projectAuthority(a, *projectID); err != nil {
			return nil, none, err
		}
	}
	if by.Author == projectcontext.AuthorTask {
		p, err := s.taskPrincipal(ctx, a)
		if err != nil {
			return nil, none, err
		}
		by.Source.TaskID, by.Source.AttemptID = &p.TaskID, &p.AttemptID
	}
	d, err := s.Queries.GetLatestDelivery(ctx, a.BindingID)
	switch {
	case err == nil:
		by.Source.DeliveryID = &d.ID
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, none, err
	}
	return &projectcontext.Service{Pool: s.Pool, OwnerID: a.UserID}, by, nil
}

func memoryFailure(err error) error {
	switch {
	case errors.Is(err, projectcontext.ErrNotFound):
		return failure("not_found", "no such project")
	case errors.Is(err, projectcontext.ErrEntryNotFound):
		return failure("not_found", "no such memory entry on this project")
	case errors.Is(err, projectcontext.ErrEntryState):
		return failure("conflict", "the memory entry is not in a state that allows this")
	case errors.Is(err, projectcontext.ErrNotYours):
		return failure("forbidden", "only the session that proposed an entry can withdraw it; ask the owner or the main session")
	case errors.Is(err, projectcontext.ErrMemoryFull):
		return failure("memory_full", fmt.Sprintf("active project memory would pass %d bytes: forget an entry, or replace one with a shorter consolidation", projectcontext.MaxMemoryBytes))
	case errors.Is(err, projectcontext.ErrTooManyProposals):
		return failure("too_many_proposals", "the project already has 20 open memory proposals for the owner")
	case errors.Is(err, projectcontext.ErrEmptyEntry), errors.Is(err, projectcontext.ErrInvalidText):
		return errValidation
	case errors.Is(err, projectcontext.ErrEntryTooLarge):
		return failure("validation", fmt.Sprintf("a memory entry is at most %d bytes", projectcontext.MaxMemoryEntryBytes))
	}
	return err
}

func entryResult(e projectcontext.Entry, line string) Result {
	m := entryMap(e)
	m["text"] = line
	return Result{Text: line, Structured: m}
}

// entryMap is an entry as structured content. Its text is under "entry", since the structured
// "text" key carries the summary line.
func entryMap(e projectcontext.Entry) map[string]any {
	m := map[string]any{
		"entry_id": e.ID, "project_id": e.ProjectID, "entry": e.Text, "state": e.State,
		"author": string(e.Author), "created_at": e.CreatedAt.UTC().Format(time.RFC3339),
	}
	if e.ReplacesID != nil {
		m["replaces"] = *e.ReplacesID
	}
	return m
}
