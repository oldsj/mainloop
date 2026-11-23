package privileged

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/attention"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// Context keys of the card. The web app sends ContextDigest back as metadata.reviewed_context
// with Approve.
const (
	ContextActionID = "privileged_action_id"
	ContextDigest   = "privileged_action_digest"
	ContextType     = "privileged_action_type"
)

// card is an action's Needs-you approval, rendered from its canonical arguments and frozen facts
// only. The agent's reason is set apart and labelled as its claim.
func card(r dbq.PrivilegedAction) (attention.NewQueueItem, error) {
	if r.Digest == nil {
		return attention.NewQueueItem{}, fmt.Errorf("privileged action %s has no digest", r.ID)
	}
	title, body, ctx, err := render(r)
	if err != nil {
		return attention.NewQueueItem{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s asks: %s.\n\n%s\n", requesterName(r), title, body)
	fmt.Fprintf(&b, "\nAgent's reason (its claim, not checked by Mainloop): %s\n", r.Reason)
	fmt.Fprintf(&b, "\nExpires %s. %s Deny, or reply with a note, refuses it and the note goes to the agent.\n",
		r.ExpiresAt.UTC().Format(time.RFC1123), catalog[r.Type].how)
	fmt.Fprintf(&b, "Action %s · digest %s", r.ID, (*r.Digest)[:16])

	ctx[ContextActionID] = r.ID
	ctx[ContextDigest] = *r.Digest
	ctx[ContextType] = r.Type
	if r.RequesterTaskID != nil {
		ctx["task_id"] = *r.RequesterTaskID
	}
	return attention.NewQueueItem{
		ID: QueueIDPrefix + r.ID, ItemType: api.QueueItemTypeApproval, Priority: api.QueueItemPriorityHigh,
		Title: title, Content: b.String(), Needed: "Approve: " + title + "?", Context: ctx,
		Options: []string{AnswerApprove, AnswerDeny},
	}, nil
}

// render is the card's title, body and context for the action's type.
func render(r dbq.PrivilegedAction) (string, string, map[string]any, error) {
	ctx := map[string]any{}
	switch r.Type {
	case TypePRMerge:
		var f MergeFacts
		if err := json.Unmarshal(r.FrozenFacts, &f); err != nil {
			return "", "", nil, err
		}
		p := f.PR
		title := fmt.Sprintf("Merge %s#%d", f.Repo, p.Number)
		var b strings.Builder
		fmt.Fprintf(&b, "%q by %s\n", p.Title, orUnknown(p.Author))
		fmt.Fprintf(&b, "Squash-merge head %s (%s) into %s, exactly at that commit: if the head moves, nothing merges.\n",
			short(p.HeadSHA), p.HeadRef, p.BaseRef)
		fmt.Fprintf(&b, "%d files changed (+%d −%d).\n", p.ChangedFiles, p.Additions, p.Deletions)
		b.WriteString(checksLine(f.Checks) + "\n")
		b.WriteString(mergeableLine(p))
		if tags := mergeTags(f); len(tags) > 0 {
			fmt.Fprintf(&b, "\nRisk: %s.", strings.Join(tags, ", "))
		}
		ctx["pr_url"] = p.HTMLURL
		ctx["repository"] = f.Repo
		return title, b.String(), ctx, nil
	case TypePRClose:
		var f CloseFacts
		if err := json.Unmarshal(r.FrozenFacts, &f); err != nil {
			return "", "", nil, err
		}
		var a CloseArgs
		if err := json.Unmarshal(r.Args, &a); err != nil {
			return "", "", nil, err
		}
		title := fmt.Sprintf("Close %s#%d", f.Repo, f.PRs[0].Number)
		if len(f.PRs) > 1 {
			title = fmt.Sprintf("Close %d pull requests in %s", len(f.PRs), f.Repo)
		}
		var b strings.Builder
		b.WriteString("Close without merging:\n")
		for _, p := range f.PRs {
			fmt.Fprintf(&b, "- #%d %q by %s (head %s, %s)\n", p.Number, p.Title, orUnknown(p.Author), short(p.HeadSHA), p.HeadRef)
		}
		if a.Comment != "" {
			fmt.Fprintf(&b, "Comment posted on each: %q\n", a.Comment)
		}
		b.WriteString("A pull request whose head moved since this was read is left open.")
		if len(f.PRs) == 1 {
			ctx["pr_url"] = f.PRs[0].HTMLURL
		}
		ctx["repository"] = f.Repo
		return title, b.String(), ctx, nil
	case TypeItemToSecret:
		return renderItemToSecret(r, ctx)
	case TypeRolloutRestart, TypeScale:
		return renderWorkload(r, ctx)
	case TypeDelete:
		return renderDelete(r, ctx)
	}
	return "", "", nil, fmt.Errorf("privileged: no card for type %q", r.Type)
}

func checksLine(c Checks) string {
	if !c.Read {
		return "Checks on the head: could not be read."
	}
	line := fmt.Sprintf("Checks on the head: %d passed, %d failed, %d pending.", c.Passed, c.Failed, c.Pending)
	if len(c.Failing) > 0 {
		line += " Failing: " + strings.Join(c.Failing, ", ") + "."
	}
	return line
}

func mergeableLine(p PRFacts) string {
	switch {
	case p.Mergeable == nil:
		return "Mergeable: GitHub had not worked it out yet."
	case *p.Mergeable:
		return "Mergeable: yes (" + p.MergeableState + ")."
	}
	return "Mergeable: no (" + p.MergeableState + "); GitHub will refuse the merge."
}

func mergeTags(f MergeFacts) []string {
	var tags []string
	if f.Checks.Failed > 0 {
		tags = append(tags, "checks failing")
	}
	if f.Checks.Pending > 0 {
		tags = append(tags, "checks pending")
	}
	if !f.Checks.Read {
		tags = append(tags, "checks unread")
	}
	if f.PR.Mergeable != nil && !*f.PR.Mergeable {
		tags = append(tags, "not mergeable")
	}
	return tags
}

func requesterName(r dbq.PrivilegedAction) string {
	switch r.RequesterRole {
	case "main":
		return "Main"
	case "supervisor":
		if r.RequesterTaskID != nil {
			return "Supervisor of task " + *r.RequesterTaskID
		}
		return "A supervisor"
	}
	return "An agent"
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func orUnknown(s string) string {
	if s == "" {
		return "an unknown author"
	}
	return s
}
