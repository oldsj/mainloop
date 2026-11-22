package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/onboarding"
)

// onboardingTools propose new projects for the owner to approve (docs/specs/projects.md#onboarding-proposals).
func (s *Server) onboardingTools() []Tool {
	str := func(minLen, maxLen int) map[string]any { return stringSchema(minLen, maxLen) }
	enum := func(values []string) map[string]any { return map[string]any{"type": "string", "enum": values} }
	return []Tool{{
		Name: agentauth.ToolProjectPropose,
		Description: "Propose making a GitHub repository a Mainloop project. The owner gets one approval card " +
			"in Needs you; nothing changes until they approve it, and approving imports the repository and applies " +
			"the settings named here. Mainloop checks whether its GitHub App is installed on the repository and " +
			"statically checks environment_image (digest-pinned, as environment registration requires); the result " +
			"says what it found, including what the owner must do when the App is not installed. services and " +
			"workspace_size are recorded on the card but not applied: Mainloop has no declared services or workspace " +
			"sizes yet. Reuse request_id to read the same proposal back; a repository with an open proposal returns that one.",
		InputSchema: objectSchema(map[string]any{
			"request_id":               str(1, onboarding.MaxRequestIDLen),
			"repository":               str(1, 300),
			"why":                      str(1, onboarding.MaxWhyLen),
			"environment_image":        str(1, onboarding.MaxImageLen),
			"environment_architecture": enum(onboarding.Architectures),
			"verification_mode":        enum(onboarding.VerificationModes),
			"workspace_service": objectSchema(map[string]any{
				"command": map[string]any{"type": "array", "items": str(1, 4096), "maxItems": 61},
				"ports": map[string]any{"type": "array", "maxItems": 16, "items": objectSchema(map[string]any{
					"name":   str(1, 32),
					"number": map[string]any{"type": "integer", "minimum": 1, "maximum": 65535},
				}, "name", "number")},
			}, "command"),
			"services":       map[string]any{"type": "array", "items": str(1, onboarding.MaxServiceLen), "maxItems": onboarding.MaxServices},
			"workspace_size": str(1, onboarding.MaxSizeLen),
		}, "request_id", "repository", "why"),
		Handle: s.projectPropose,
	}}
}

func (s *Server) projectPropose(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	var in struct {
		RequestID string `json:"request_id"`
		onboarding.Spec
	}
	if err := decode(raw, &in); err != nil {
		return Result{}, err
	}
	if s.Onboarding == nil {
		return Result{}, failure("unavailable", "project proposals are not available")
	}
	by := onboarding.Proposer{Role: string(a.Role), SessionID: &a.BindingID}
	if a.Role == dbstate.RoleSupervisor {
		p, err := s.taskPrincipal(ctx, a)
		if err != nil {
			return Result{}, err
		}
		by.TaskID = &p.TaskID
	}
	svc := *s.Onboarding
	svc.OwnerID = a.UserID
	p, err := svc.Propose(ctx, by, in.RequestID, in.Spec)
	switch {
	case errors.Is(err, onboarding.ErrInvalid):
		return Result{}, failure("validation", strings.TrimPrefix(err.Error(), onboarding.ErrInvalid.Error()+": "))
	case errors.Is(err, onboarding.ErrConflict):
		return Result{}, failure("conflict", "request_id was already used for a different proposal; use a new request_id")
	case err != nil:
		return Result{}, err
	}
	return proposalResult(p), nil
}

func proposalResult(p onboarding.Proposal) Result {
	var b strings.Builder
	switch {
	case p.Replayed && p.State == onboarding.StateProposed:
		fmt.Fprintf(&b, "Proposal %s for %s already waits for the owner (Needs you).", p.ID, p.Repository)
	case p.Replayed:
		fmt.Fprintf(&b, "Proposal %s for %s is %s.", p.ID, p.Repository, p.State)
	default:
		fmt.Fprintf(&b, "Proposal %s for %s waits for the owner's approval in Needs you.", p.ID, p.Repository)
	}
	switch p.Facts.GitHubApp {
	case onboarding.AppInstalled:
		b.WriteString(" The GitHub App is installed on the repository.")
	case onboarding.AppNotInstalled:
		b.WriteString(" The GitHub App is NOT installed on the repository; tell the owner: " + onboarding.InstallHint(p.Repository))
	default:
		b.WriteString(" The GitHub App installation was not checked: " + p.Facts.GitHubAppDetail + ".")
	}
	if p.Facts.ProjectID != nil {
		fmt.Fprintf(&b, " The repository is already project %s; approving applies the settings to it.", *p.Facts.ProjectID)
	}
	if len(p.Spec.Services) > 0 || p.Spec.WorkspaceSize != "" {
		b.WriteString(" services and workspace_size are recorded on the card, not applied.")
	}
	line := b.String()
	m := map[string]any{
		"text": line, "proposal_id": p.ID, "request_id": p.RequestID, "repository": p.Repository,
		"state": p.State, "replayed": p.Replayed, "queue_item_id": p.QueueItemID,
		"inbox_url": "/inbox?item=" + p.QueueItemID, "github_app": p.Facts.GitHubApp,
		"created_at": p.CreatedAt.UTC().Format(time.RFC3339),
	}
	if p.Facts.GitHubAppDetail != "" {
		m["github_app_detail"] = p.Facts.GitHubAppDetail
	}
	if p.Facts.ProjectID != nil {
		m["existing_project_id"] = *p.Facts.ProjectID
	}
	if p.Facts.EnvironmentDigest != "" {
		m["environment_digest"] = p.Facts.EnvironmentDigest
	}
	if p.ProjectID != nil {
		m["project_id"] = *p.ProjectID
	}
	return Result{Text: line, Structured: m}
}
