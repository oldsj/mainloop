package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/ownermemory"
	"github.com/oldsj/mainloop/server/internal/projectcontext"
)

// Memory scopes. Project memory is the default, so a caller that forgets project_id gets a
// validation error rather than a silent owner-level write.
const (
	scopeProject = "project"
	scopeOwner   = "owner"
)

const rememberWhatBelongs = "Owner scope holds the owner's own words about how they work, true across every project: " +
	"never a fact Mainloop records (task or PR state, CI, transcripts, projects, cluster or workspace state), never " +
	"a repository fact, never a line about one project, never a credential, nothing about another person. "

// memoryTools record and forget project and owner memory (docs/specs/project-context.md,
// docs/specs/owner-memory.md). The server resolves where an entry came from; the agent only says
// what to remember. Workers and workspace agents cannot record owner memory, so the descriptions
// they are listed leave the owner scope out; the schema stays the same, so a call with it is
// refused as forbidden rather than as malformed.
func (s *Server) memoryTools() []Tool {
	rememberProject := "Record a short lesson for every later agent on the project, such as an owner preference or a " +
		"verified fact that is not about the repository's code (that belongs in an AGENTS.md change). At most 500 " +
		"bytes, on one line. The main session's entries are active at once and go into every new task brief; " +
		"only record the owner's own words or facts you verified. Any other session's entry is a proposal the " +
		"owner accepts or rejects. Set replaces to the id of an active entry the new one supersedes. " +
		"Recording text that is already open answers that entry."
	forgetProject := "End a project memory entry. The main session ends an active entry that turned out wrong or " +
		"out of date; any other session withdraws a proposal it made itself."
	return []Tool{{
		Name: agentauth.ToolRemember,
		Description: rememberProject + " With scope owner (main and supervisors only; omit project_id), record " +
			"owner memory instead: loaded into every session the owner has, at most 3072 bytes in all. " + rememberWhatBelongs +
			"Main's owner entries are active at once (record only the owner's own words; when the owner contradicts " +
			"an entry, record the new wording with replaces); a supervisor's are proposals. On memory_full, ask the " +
			"owner which entry to drop or how to consolidate, never choose silently.",
		InputSchema: objectSchema(map[string]any{
			"text":       stringSchema(1, projectcontext.MaxMemoryEntryBytes),
			"scope":      map[string]any{"type": "string", "enum": []any{scopeProject, scopeOwner}},
			"project_id": stringSchema(1, 100),
			"replaces":   stringSchema(1, 100),
		}, "text"),
		Limited: &Listing{For: ownerScopeForbidden, Description: rememberProject},
		Handle:  s.remember,
	}, {
		Name: agentauth.ToolForget,
		Description: forgetProject + " With scope owner (main and supervisors only; omit project_id), the same for " +
			"an owner memory entry: main forgets an active one, a supervisor withdraws its own proposal.",
		InputSchema: objectSchema(map[string]any{
			"entry_id":   stringSchema(1, 100),
			"scope":      map[string]any{"type": "string", "enum": []any{scopeProject, scopeOwner}},
			"project_id": stringSchema(1, 100),
		}, "entry_id"),
		Limited: &Listing{For: ownerScopeForbidden, Description: forgetProject},
		Handle:  s.forget,
	}, {
		Name: agentauth.ToolOwnerMemory,
		Description: "Read the owner's memory: their own standing preferences across every project, which every " +
			"session is given (a brief carries them as they were when it started). Answers the active entries with " +
			"the date each was last confirmed, the bytes used of 3072, and how many proposals and entries due for " +
			"review wait for the owner. Facts (tasks, PRs, CI, transcripts, projects) are never here: look them up " +
			"with the tools that read them.",
		InputSchema: objectSchema(map[string]any{}),
		Handle:      s.ownerMemory,
	}}
}

// ownerScopeForbidden is who may not record owner memory: every role but main and supervisors.
func ownerScopeForbidden(a agentauth.Actor) bool {
	return a.Role != dbstate.RoleMain && a.Role != dbstate.RoleSupervisor
}

// memoryScope checks scope against project_id: project (the default) needs one, owner takes none.
func memoryScope(scope, projectID string) (string, error) {
	switch scope {
	case "", scopeProject:
		if projectID == "" {
			return "", failure("validation", "project memory needs project_id; for owner memory pass scope owner")
		}
		return scopeProject, nil
	case scopeOwner:
		if projectID != "" {
			return "", failure("validation", "owner memory takes no project_id")
		}
		return scopeOwner, nil
	}
	return "", failure("validation", "scope is project or owner")
}

func (s *Server) remember(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	var in struct {
		ProjectID string `json:"project_id"`
		Text      string `json:"text"`
		Replaces  string `json:"replaces"`
		Scope     string `json:"scope"`
	}
	if err := decode(raw, &in); err != nil {
		return Result{}, err
	}
	scope, err := memoryScope(in.Scope, in.ProjectID)
	if err != nil {
		return Result{}, err
	}
	if scope == scopeOwner {
		svc, by, err := s.ownerMemoryActor(ctx, a)
		if err != nil {
			return Result{}, err
		}
		e, err := svc.Remember(ctx, by, in.Text, in.Replaces)
		if err != nil {
			return Result{}, ownerMemoryFailure(err)
		}
		line := fmt.Sprintf("Owner memory %s is %s.", e.ID, e.State)
		switch e.State {
		case ownermemory.StateActive:
			line += " Every session the owner has is given it."
		case ownermemory.StateProposed:
			line += " The owner decides whether sessions get it."
		}
		return ownerEntryResult(e, line), nil
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
		Scope     string `json:"scope"`
	}
	if err := decode(raw, &in); err != nil {
		return Result{}, err
	}
	var err error
	if in.EntryID, err = text(in.EntryID, 1, 100); err != nil {
		return Result{}, err
	}
	scope, err := memoryScope(in.Scope, in.ProjectID)
	if err != nil {
		return Result{}, err
	}
	if scope == scopeOwner {
		svc, by, err := s.ownerMemoryActor(ctx, a)
		if err != nil {
			return Result{}, err
		}
		e, err := svc.Forget(ctx, by, in.EntryID)
		if err != nil {
			return Result{}, ownerMemoryFailure(err)
		}
		return ownerEntryResult(e, fmt.Sprintf("Owner memory %s is %s.", e.ID, e.State)), nil
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

func (s *Server) ownerMemory(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	var in struct{}
	if err := decode(raw, &in); err != nil {
		return Result{}, err
	}
	if s.Pool == nil {
		return Result{}, failure("unavailable", "owner memory is not available")
	}
	svc := &ownermemory.Service{Pool: s.Pool, OwnerID: a.UserID, Now: s.Now}
	mem, err := svc.Memory(ctx)
	if err != nil {
		return Result{}, err
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	active, proposed, due := []map[string]any{}, 0, 0
	var b strings.Builder
	for _, e := range mem.Entries {
		switch e.State {
		case ownermemory.StateProposed:
			proposed++
		case ownermemory.StateActive:
			if d := e.DueAt(mem.ReviewAfterDays); d != nil && !now.Before(*d) {
				due++
			}
			confirmed := e.LastConfirmedAt.UTC()
			active = append(active, map[string]any{
				"entry_id": e.ID, "text": e.Text, "last_confirmed_at": confirmed.Format(time.RFC3339),
			})
			fmt.Fprintf(&b, "\n- %s (owner memory %s, confirmed %s)", e.Text, e.ID, confirmed.Format(time.DateOnly))
		}
	}
	line := "No active owner memory."
	if len(active) > 0 {
		line = fmt.Sprintf("Owner memory (%d of %d bytes):%s", mem.UsedBytes, ownermemory.MaxBytes, b.String())
	}
	if proposed > 0 || due > 0 {
		line += fmt.Sprintf("\nWaiting for the owner: %d proposals, %d entries due for review.", proposed, due)
	}
	m := map[string]any{
		"entries": active, "used_bytes": mem.UsedBytes, "max_bytes": ownermemory.MaxBytes,
		"proposed": proposed, "due": due, "text": line,
	}
	return Result{Text: line, Structured: m}, nil
}

// ownerMemoryActor resolves who records owner memory: main (active at once) or a supervisor (a
// proposal), with the session, task, attempt and latest delivery it is acting in. Workers and
// workspace agents cannot record it.
func (s *Server) ownerMemoryActor(ctx context.Context, a agentauth.Actor) (*ownermemory.Service, ownermemory.Recorder, error) {
	var none ownermemory.Recorder
	if ownerScopeForbidden(a) {
		return nil, none, failure("forbidden", "only the main session and supervisors record owner memory; say in your report if an entry looks wrong")
	}
	if s.Pool == nil {
		return nil, none, failure("unavailable", "owner memory is not available")
	}
	by := ownermemory.Recorder{Author: ownermemory.AuthorMain, Source: projectcontext.Source{SessionID: &a.BindingID}}
	if a.Role == dbstate.RoleSupervisor {
		by.Author = ownermemory.AuthorTask
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
	return &ownermemory.Service{Pool: s.Pool, OwnerID: a.UserID, Now: s.Now}, by, nil
}

func ownerMemoryFailure(err error) error {
	switch {
	case errors.Is(err, ownermemory.ErrEntryNotFound):
		return failure("not_found", "no such owner memory entry")
	case errors.Is(err, ownermemory.ErrEntryState):
		return failure("conflict", "the owner memory entry is not in a state that allows this")
	case errors.Is(err, ownermemory.ErrNotYours):
		return failure("forbidden", "only the session that proposed an entry can withdraw it; ask the owner or the main session")
	case errors.Is(err, ownermemory.ErrMemoryFull):
		return failure("memory_full", fmt.Sprintf("active owner memory would pass %d bytes: ask the owner which entry to forget, "+
			"or replace two entries with one shorter line; do not choose silently", ownermemory.MaxBytes))
	case errors.Is(err, ownermemory.ErrTooManyProposals):
		return failure("too_many_proposals", fmt.Sprintf("the owner already has %d open owner memory proposals", ownermemory.MaxOpenProposals))
	case errors.Is(err, ownermemory.ErrSecret):
		return failure("validation", "the entry looks like a credential; owner memory never holds secrets")
	}
	return memoryFailure(err)
}

func ownerEntryResult(e ownermemory.Entry, line string) Result {
	m := map[string]any{
		"entry_id": e.ID, "entry": e.Text, "state": e.State, "author": string(e.Author), "scope": scopeOwner,
		"created_at": e.CreatedAt.UTC().Format(time.RFC3339), "text": line,
	}
	if e.ReplacesID != nil {
		m["replaces"] = *e.ReplacesID
	}
	if e.LastConfirmedAt != nil {
		m["last_confirmed_at"] = e.LastConfirmedAt.UTC().Format(time.RFC3339)
	}
	return Result{Text: line, Structured: m}
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
	m["text"], m["scope"] = line, scopeProject
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
