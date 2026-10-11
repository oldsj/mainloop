package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/topics"
)

// coordinationTools are the tools of the owner's notebook and the actor's own identity: whoami,
// topics, topic_open, note, decide, pending_add and pending_done.
func (s *Server) coordinationTools() []Tool {
	none := objectSchema(map[string]any{})
	record := objectSchema(map[string]any{
		"text":  stringSchema(1, topics.MaxChars),
		"topic": optional(stringSchema(1, 80)),
	}, "text")
	return []Tool{
		{
			Name:        agentauth.ToolWhoami,
			Description: "Show your non-secret binding identity, grant status and resolved workspace scope.",
			InputSchema: none, Handle: s.whoami,
		},
		{
			Name: agentauth.ToolTopics, Description: "List topics and pending counts.",
			InputSchema: none, Handle: s.topicIndex,
		},
		{
			Name: agentauth.ToolTopicOpen, Description: "Create or select a topic and set its status.",
			InputSchema: objectSchema(map[string]any{
				"name":   stringSchema(1, 80),
				"status": optional(stringSchema(0, topics.MaxChars)),
			}, "name"),
			Handle: s.topicOpen,
		},
		{
			Name: agentauth.ToolNote, Description: "Write a durable topic note.",
			InputSchema: record, Handle: s.record(dbstate.RecordNote),
		},
		{
			Name: agentauth.ToolDecide, Description: "Record a topic decision.",
			InputSchema: record, Handle: s.record(dbstate.RecordDecision),
		},
		{
			Name: agentauth.ToolPendingAdd, Description: "Record pending user intent.",
			InputSchema: record, Handle: s.record(dbstate.RecordPending),
		},
		{
			Name: agentauth.ToolPendingDone, Description: "Close a pending item by id or unique prefix.",
			InputSchema: objectSchema(map[string]any{"id": stringSchema(topics.MinPrefix, 0)}, "id"),
			Handle:      s.pendingDone,
		},
	}
}

func (s *Server) whoami(ctx context.Context, a agentauth.Actor, args json.RawMessage) (Result, error) {
	var in struct{}
	if err := decode(args, &in); err != nil {
		return Result{}, err
	}
	grantStatus, scopeStatus := "active", "not_enrolled"
	var repository, branch, workspaceID *string
	if a.Grant == dbstate.GrantWorkspace {
		id := a.BindingID
		workspaceID = &id
		scopeStatus = "available"
		repo, br, err := s.workspaceScope(ctx, a)
		if err != nil {
			return Result{}, err
		}
		if repo == "" || br == "" {
			grantStatus, scopeStatus = "scope_unavailable", "scope_unavailable"
		} else {
			repository, branch = &repo, &br
		}
	}
	var projectID *string
	if a.ProjectID != "" {
		projectID = &a.ProjectID
	}
	short := a.BindingID[:min(8, len(a.BindingID))]
	return Result{
		Text: fmt.Sprintf("%s %s session=%s depth=%d grant=%s scope=%s",
			a.Role, a.AgentKind, short, a.Depth, grantStatus, scopeStatus),
		Structured: map[string]any{
			"session_id": a.BindingID, "role": string(a.Role), "depth": a.Depth,
			"mcp_grant_kind": string(a.Grant), "grant_status": grantStatus, "scope_status": scopeStatus,
			"project_id": projectID, "workspace_id": workspaceID, "repository": repository, "branch": branch,
		},
	}, nil
}

// workspaceScope reads the repository and branch a workspace agent works on. Blank values mean
// the scope cannot be resolved.
func (s *Server) workspaceScope(ctx context.Context, a agentauth.Actor) (repository, branch string, err error) {
	sess, err := s.Queries.GetSession(ctx, a.BindingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read session: %w", err)
	}
	if sess.ProjectID == nil || sess.BranchName == nil {
		return "", "", nil
	}
	project, err := s.Queries.GetProject(ctx, *sess.ProjectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read project: %w", err)
	}
	return project.FullName, *sess.BranchName, nil
}

func (s *Server) topicIndex(ctx context.Context, a agentauth.Actor, args json.RawMessage) (Result, error) {
	var in struct{}
	if err := decode(args, &in); err != nil {
		return Result{}, err
	}
	index, err := s.Topics.Index(ctx, a.UserID)
	if err != nil {
		return Result{}, err
	}
	lines := make([]string, len(index))
	for i, t := range index {
		status := t.StatusLine
		if status == "" {
			status = "(no status)"
		}
		lines[i] = fmt.Sprintf("- %s: %s [%d pending]", t.Name, status, t.Pending)
	}
	text := strings.Join(lines, "\n")
	if text == "" {
		text = "(no topics yet)"
	}
	return Result{Text: text, Structured: map[string]any{"topics": index}}, nil
}

func (s *Server) topicOpen(ctx context.Context, a agentauth.Actor, args json.RawMessage) (Result, error) {
	var in struct {
		Name   string  `json:"name"`
		Status *string `json:"status"`
	}
	if err := decode(args, &in); err != nil {
		return Result{}, err
	}
	name, err := text(in.Name, 1, 80)
	if err != nil {
		return Result{}, err
	}
	var status *string
	if in.Status != nil {
		v, err := text(*in.Status, 0, topics.MaxChars)
		if err != nil {
			return Result{}, err
		}
		status = &v
	}
	opened, err := s.Topics.Open(ctx, a.UserID, name, status)
	if err != nil {
		return Result{}, err
	}
	return Result{Text: "topic " + opened + " ready", Structured: map[string]any{"topic": opened}}, nil
}

// record builds the handler of note, decide and pending_add, which differ only in kind.
func (s *Server) record(kind dbstate.TopicRecordKind) func(context.Context, agentauth.Actor, json.RawMessage) (Result, error) {
	return func(ctx context.Context, a agentauth.Actor, args json.RawMessage) (Result, error) {
		var in struct {
			Text  string  `json:"text"`
			Topic *string `json:"topic"`
		}
		if err := decode(args, &in); err != nil {
			return Result{}, err
		}
		body, err := text(in.Text, 1, topics.MaxChars)
		if err != nil {
			return Result{}, err
		}
		topic := ""
		if in.Topic != nil {
			if topic, err = text(*in.Topic, 1, 80); err != nil {
				return Result{}, err
			}
		}
		rec, err := s.Topics.Add(ctx, a.UserID, a.BindingID, kind, body, topic)
		if err != nil {
			return Result{}, err
		}
		return Result{
			Text:       fmt.Sprintf("%s recorded in %s (%s)", kind, rec.Topic, rec.ID[:min(8, len(rec.ID))]),
			Structured: map[string]any{"id": rec.ID},
		}, nil
	}
}

func (s *Server) pendingDone(ctx context.Context, a agentauth.Actor, args json.RawMessage) (Result, error) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decode(args, &in); err != nil {
		return Result{}, err
	}
	id := strings.TrimSpace(in.ID)
	if len([]rune(id)) < topics.MinPrefix {
		return Result{}, errValidation
	}
	switch err := s.Topics.ClosePending(ctx, a.UserID, id); {
	case errors.Is(err, topics.ErrNoPending):
		return Result{}, failure("404", "no such open pending item")
	case errors.Is(err, topics.ErrAmbiguousPrefix):
		return Result{}, failure("409", "the pending id prefix matches more than one item")
	case err != nil:
		return Result{}, err
	}
	return Result{Text: "pending closed", Structured: map[string]any{}}, nil
}
