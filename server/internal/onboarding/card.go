package onboarding

import (
	"fmt"
	"strings"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/attention"
)

// InstallHint is what the owner is told to do when the GitHub App is not installed on the
// repository. Mainloop does not know its App's public name, so the hint names the settings page.
func InstallHint(repository string) string {
	return fmt.Sprintf("Install Mainloop's GitHub App on %s first (GitHub → Settings → Applications → "+
		"Installed GitHub Apps → Configure → Repository access), then approve. Without it, workspaces "+
		"cannot clone or push.", repository)
}

// card is the proposal's Needs-you item: an approval with two answers, its full text in Content
// and one line in Needed.
func card(id string, by Proposer, spec Spec, f Facts) attention.NewQueueItem {
	needed := fmt.Sprintf("Approve onboarding %s as a Mainloop project?", spec.Repository)
	if f.GitHubApp == AppNotInstalled {
		needed += " The GitHub App is not installed on it yet."
	}
	ctx := map[string]any{"project_proposal_id": id, "repository": spec.Repository}
	if f.ProjectID != nil {
		ctx["project_id"] = *f.ProjectID
	}
	return attention.NewQueueItem{
		ID: QueueIDPrefix + id, ItemType: api.QueueItemTypeApproval, Priority: api.QueueItemPriorityNormal,
		Title:   "Onboard " + spec.Repository,
		Content: cardText(by, spec, f), Needed: needed, Context: ctx,
		Options: []string{AnswerApprove, AnswerReject},
	}
}

// cardText says what was proposed, what Mainloop found, and what each answer does.
func cardText(by Proposer, spec Spec, f Facts) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s proposes making %s a Mainloop project.\n", proposerName(by), spec.Repository)
	fmt.Fprintf(&b, "Why: %s\n\n", spec.Why)

	switch f.GitHubApp {
	case AppInstalled:
		b.WriteString("GitHub App: installed on the repository.\n")
	case AppNotInstalled:
		b.WriteString("GitHub App: NOT installed. " + InstallHint(spec.Repository) + "\n")
	default:
		b.WriteString("GitHub App: not checked (" + f.GitHubAppDetail + ").\n")
	}
	if f.ProjectID != nil {
		b.WriteString("Project: already a project; approving applies the settings below to it.\n")
	} else {
		b.WriteString("Project: new; approving imports it.\n")
	}
	if spec.EnvironmentImage != "" {
		fmt.Fprintf(&b, "Environment: %s (%s, static check passed", spec.EnvironmentImage, spec.EnvironmentArchitecture)
		if f.EnvironmentDigest != "" {
			fmt.Fprintf(&b, ", platform manifest %s", f.EnvironmentDigest)
		}
		b.WriteString(").\n")
	} else {
		b.WriteString("Environment: unchanged (the default).\n")
	}
	if spec.VerificationMode != "" {
		fmt.Fprintf(&b, "Checks: %s.\n", spec.VerificationMode)
	} else {
		b.WriteString("Checks: unchanged (Automatic for a new project).\n")
	}
	if ws := spec.WorkspaceService; ws != nil {
		cmd := "none"
		if len(ws.Command) > 0 {
			cmd = strings.Join(ws.Command, " ")
		}
		ports := make([]string, 0, len(ws.Ports))
		for _, p := range ws.Ports {
			ports = append(ports, fmt.Sprintf("%s=%d", p.Name, p.Number))
		}
		fmt.Fprintf(&b, "Workspace service: %s", cmd)
		if len(ports) > 0 {
			fmt.Fprintf(&b, "; ports %s", strings.Join(ports, ", "))
		}
		b.WriteString(".\n")
	}
	var recorded []string
	if len(spec.Services) > 0 {
		recorded = append(recorded, "services "+strings.Join(spec.Services, ", "))
	}
	if spec.WorkspaceSize != "" {
		recorded = append(recorded, "workspace size "+spec.WorkspaceSize)
	}
	if len(recorded) > 0 {
		fmt.Fprintf(&b, "Recorded, not applied (Mainloop has no declared services or workspace sizes yet): %s.\n",
			strings.Join(recorded, "; "))
	}
	b.WriteString("\nApprove imports the repository and applies these settings. Reject changes nothing.")
	return b.String()
}

func proposerName(by Proposer) string {
	switch by.Role {
	case "main":
		return "Main"
	case "supervisor":
		if by.TaskID != nil {
			return "Supervisor task " + *by.TaskID
		}
		return "A supervisor"
	}
	return "An agent"
}
