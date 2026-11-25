package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/privileged"
)

// privilegedTools request credentialed actions the owner approves from one card
// (docs/specs/privileged-actions.md).
func (s *Server) privilegedTools() []Tool {
	str := func(minLen, maxLen int) map[string]any { return stringSchema(minLen, maxLen) }
	return []Tool{{
		Name: agentauth.ToolRequestPrivilegedAction,
		Description: "Ask the owner to approve a credentialed action you cannot run yourself. Mainloop checks the target " +
			"(preflight), freezes what it found and puts one approval card in the owner's Needs you, pushed to their phone; the " +
			"card shows the exact target and facts, and your reason labelled as your claim. Nothing runs until the owner approves " +
			"that exact card; then Mainloop runs it once and sends you one message with the outcome (succeeded, failed, stale, " +
			"denied with the owner's note, expired or uncertain). End your turn after requesting; do not poll. Types: " +
			"github.pr_merge {repo: \"owner/name\", pr, expected_head_sha} squash-merges exactly that head through Mainloop's " +
			"GitHub App and never bypasses a branch rule; github.pr_close {repo, prs: [numbers, at most 20], comment?} closes " +
			"open pull requests without merging; onepassword.item_to_secret {vault, item, field (each an id or exact title), " +
			"namespace, secret, key} copies one 1Password field into one key of a Kubernetes Secret through Mainloop's executor " +
			"(you never see the value; only the namespaces the executor allows; answers unavailable where no executor runs); " +
			"k8s.rollout_restart {namespace, kind: Deployment|StatefulSet|DaemonSet, name} restarts its pods as kubectl rollout " +
			"restart does; k8s.scale {namespace, kind: Deployment|StatefulSet, name, replicas: 0-100} sets its replicas; " +
			"k8s.delete {namespace (empty for a cluster-scoped object), kind (Pod, Job.batch, Certificate.cert-manager.io), name} " +
			"deletes that one object (its card expires in an hour); k8s.patch {namespace, kind, name, patch_type: merge|strategic|json, " +
			"patch (an object, or an array of JSON patch operations)} patches one object as kubectl patch does; k8s.apply " +
			"{manifests (YAML or JSON, up to 50 objects, 64 KiB), namespace (the default for objects naming none), force_conflicts} " +
			"server-side applies them. Patch and apply show the owner the API server's dry-run diff (16 KiB at most, never status " +
			"or server-kept metadata, which they may not set) and write only while it still holds; you get the objects' names, " +
			"not the diff (no JSON patch test ops; setting or showing a credential is refused, naming the field; 5 refused preflights an hour per session, then too_many_refused). They refuse Secrets (use onepassword.item_to_secret). argocd.sync {app, namespace (default argocd), prune} starts " +
			"an Argo CD sync of that Application at the revision Argo CD resolved, with its own sync options and retry, only while Git, its spec and the out-of-sync set stay as the " +
			"owner saw them (refused for an app with resources in the executor's namespace; with prune its card expires in an hour); it succeeds once the sync starts. command.kubectl {target (the executor's cluster), argv (kubectl and its words, no shell; " +
			"at most 64 words, 8 KiB), stdin (for -f - or exec -i; 64 KiB), timeout_seconds (default 60, at most 600)} runs that " +
			"one command once as cluster-admin, for what no typed action covers: the card shows the exact command and, for a change, " +
			"the API server's dry-run diff (or a delete's dry-run text), run again before the command; you get the objects' names, not " +
			"the diff, and the output, redacted. argv is read by kubectl's own parser; each allowed command has its own allowed flags and " +
			"the refusal names them. Refused: other targets or credentials, impersonation, local files (-f takes only -), Secrets, " +
			"create secret or token, --raw, --dry-run, --prune, watches, -A or --all on a change, admission policies and webhooks, a " +
			"flag value starting with -, and edit, cp, proxy, port-forward, attach, debug, config and plugins. kubectl never holds the " +
			"credential: the executor's proxy does, and refuses those whatever kubectl sends. A failed dry run gives its reason " +
			"(NotFound), not its text. A lost answer is uncertain, never " +
			"run again; a delete's card expires in an hour. The k8s.* types, argocd.sync and command.kubectl run through the " +
			"executor on the cluster it runs in, never in its own namespace, and answer unavailable where it has no Kubernetes " +
			"access; prefer a Git change for anything Argo CD manages, and a typed action where one fits. " +
			"command.omnictl {argv, stdin (for apply --file=-), timeout_seconds} and command.talosctl {target (an Omni cluster the " +
			"executor may act on), argv (with --nodes=<node>), timeout_seconds} run one omnictl or talosctl command once through Omni " +
			"as the executor's service account (role Admin). Allowed: omnictl get, cluster status, apply (the card shows " +
			"its --dry-run diff, run again before it), delete and cluster delete; talosctl version, get, service, logs, etcd " +
			"members|status|alarm list, containers, stats, memory, processes, mounts, netstat, time, list, and (no preview) reboot, " +
			"shutdown, reset, upgrade (--wait=false on these four; --image the node's own installer, factory.talos.dev/<platform>-installer/<the schematic ID Omni records for it>:<a version no older than it runs>; give " +
			"an upgrade timeout_seconds for the image's pull and install), " +
			"rollback, service <id> start|stop|restart, etcd defrag|forfeit-leadership|leave|remove-member|alarm disarm. omnictl " +
			"delete takes <type> <id>, no selector. Flags are long only, as --name or --name=value; the refusal names the allowed " +
			"ones. get and delete take only the resource types an operator reads (clusters, machine sets, machines and statuses; " +
			"Talos members, addresses, routes, links, disks, volumes, services), by full, plural or singular name; the refusal lists " +
			"them. Refused: endpoint, config, context, cluster and credential flags, watches and follows, dmesg and machine-logs " +
			"(the kernel log holds the join token), other types (config patches, configs, tokens, secrets, kernel args, machine " +
			"statuses), and types the server marks sensitive. omnictl is held to the executor's clusters: " +
			"it changes only resources labelled omni.sidero.dev/cluster=<one of them> and sees none of another cluster's; talosctl " +
			"reaches only its target's machines. The tools never hold the Omni key: the executor's proxy signs each request and " +
			"refuses those whatever they send. " +
			"Deletes, resets, shutdowns and etcd removals expire in an hour; they answer unavailable where the executor has no Omni access. " +
			"An executor type returns state requested; its card opens once the executor has checked the target. Reuse request_id to read the same request back. At most 3 requests per session " +
			"and 10 in all wait for the owner.",
		InputSchema: objectSchema(map[string]any{
			"request_id": str(1, privileged.MaxRequestIDLen),
			"type":       map[string]any{"type": "string", "enum": privileged.Types()},
			"args":       map[string]any{"type": "object"},
			"reason":     str(1, privileged.MaxReasonLen),
		}, "request_id", "type", "args", "reason"),
		Handle: s.requestPrivilegedAction,
	}, {
		Name:        agentauth.ToolGetPrivilegedAction,
		Description: "Read a privileged action you requested: its state, frozen facts (for k8s.patch and k8s.apply, the objects' names only, not the diffs), the owner's decision, the result and the audit events.",
		InputSchema: objectSchema(map[string]any{"action_id": str(1, 100)}, "action_id"),
		Handle:      s.getPrivilegedAction,
	}, {
		Name:        agentauth.ToolCancelPrivilegedAction,
		Description: "Withdraw a privileged action you requested that the owner has not decided yet; its card closes.",
		InputSchema: objectSchema(map[string]any{"action_id": str(1, 100)}, "action_id"),
		Handle:      s.cancelPrivilegedAction,
	}}
}

func (s *Server) privilegedService(a agentauth.Actor) (*privileged.Service, error) {
	if s.Privileged == nil {
		return nil, failure("unavailable", "privileged actions are not enabled on this Mainloop; report blocked with the exact "+
			"command for the owner to run")
	}
	svc := *s.Privileged
	svc.OwnerID = a.UserID
	return &svc, nil
}

func (s *Server) requestPrivilegedAction(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	var in struct {
		RequestID string          `json:"request_id"`
		Type      string          `json:"type"`
		Args      json.RawMessage `json:"args"`
		Reason    string          `json:"reason"`
	}
	if err := decode(raw, &in); err != nil {
		return Result{}, err
	}
	svc, err := s.privilegedService(a)
	if err != nil {
		return Result{}, err
	}
	by := privileged.Requester{Role: string(a.Role), SessionID: a.BindingID}
	if a.Role == dbstate.RoleSupervisor {
		p, err := s.taskPrincipal(ctx, a)
		if err != nil {
			return Result{}, err
		}
		by.TaskID = &p.TaskID
	}
	act, err := svc.Request(ctx, by, in.Type, in.Args, in.Reason, in.RequestID)
	if err != nil {
		return Result{}, privilegedFailure(err)
	}
	return privilegedResult(act, nil), nil
}

func (s *Server) getPrivilegedAction(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	var in struct {
		ActionID string `json:"action_id"`
	}
	if err := decode(raw, &in); err != nil {
		return Result{}, err
	}
	svc, err := s.privilegedService(a)
	if err != nil {
		return Result{}, err
	}
	act, evs, err := svc.Get(ctx, a.BindingID, in.ActionID)
	if err != nil {
		return Result{}, privilegedFailure(err)
	}
	return privilegedResult(act, evs), nil
}

func (s *Server) cancelPrivilegedAction(ctx context.Context, a agentauth.Actor, raw json.RawMessage) (Result, error) {
	var in struct {
		ActionID string `json:"action_id"`
	}
	if err := decode(raw, &in); err != nil {
		return Result{}, err
	}
	svc, err := s.privilegedService(a)
	if err != nil {
		return Result{}, err
	}
	act, err := svc.Cancel(ctx, a.BindingID, in.ActionID)
	if err != nil {
		return Result{}, privilegedFailure(err)
	}
	return privilegedResult(act, nil), nil
}

func privilegedFailure(err error) error {
	trim := func(sentinel error) string { return strings.TrimPrefix(err.Error(), sentinel.Error()+": ") }
	switch {
	case errors.Is(err, privileged.ErrInvalid):
		return failure("validation", trim(privileged.ErrInvalid))
	case errors.Is(err, privileged.ErrUnsupported):
		return failure("unsupported_action", trim(privileged.ErrUnsupported)+"; report blocked with the exact command for the owner to run")
	case errors.Is(err, privileged.ErrConflict):
		return failure("conflict", "request_id was already used for a different action; use a new request_id")
	case errors.Is(err, privileged.ErrTooManyPending):
		return failure("too_many_pending", trim(privileged.ErrTooManyPending))
	case errors.Is(err, privileged.ErrTooManyRefused):
		return failure("too_many_refused", trim(privileged.ErrTooManyRefused))
	case errors.Is(err, privileged.ErrNotFound):
		return failure("not_found", "no such privileged action of this session")
	case errors.Is(err, privileged.ErrNotCancellable):
		return failure("conflict", "the owner already decided this action, or it ended; it cannot be cancelled")
	}
	return err
}

func privilegedResult(act privileged.Action, evs []dbq.PrivilegedActionEvent) Result {
	var b strings.Builder
	switch act.State {
	case privileged.StateRequested:
		fmt.Fprintf(&b, "Privileged action %s is requested; Mainloop could not read the target yet and will retry", act.ID)
		if act.LastError != nil {
			b.WriteString(" (" + *act.LastError + ")")
		}
		b.WriteString(". You get one message when it ends; end your turn.")
	case privileged.StatePendingApproval:
		fmt.Fprintf(&b, "Privileged action %s waits for the owner's approval in Needs you (expires %s). You get one message "+
			"with the outcome; end your turn now, do not poll.", act.ID, act.ExpiresAt.UTC().Format(time.RFC3339))
	case privileged.StatePreflightFailed:
		fmt.Fprintf(&b, "Privileged action %s was not put to the owner: %s.", act.ID, deref(act.LastError))
	default:
		fmt.Fprintf(&b, "Privileged action %s is %s.", act.ID, act.State)
		if act.DecisionNote != nil {
			b.WriteString(" The owner's note: " + *act.DecisionNote)
		}
		if act.LastError != nil && act.State != privileged.StateSucceeded {
			b.WriteString(" Detail: " + *act.LastError)
		}
	}
	if act.Replayed {
		b.WriteString(" (An earlier request with the same request_id or arguments.)")
	}
	line := b.String()
	m := map[string]any{
		"text": line, "action_id": act.ID, "request_id": act.RequestID, "type": act.Type, "state": act.State,
		"replayed": act.Replayed, "expires_at": act.ExpiresAt.UTC().Format(time.RFC3339),
		"created_at": act.CreatedAt.UTC().Format(time.RFC3339), "args": act.Args,
	}
	if act.Digest != "" {
		m["digest"] = act.Digest
		m["queue_item_id"] = act.QueueItemID
		m["inbox_url"] = "/inbox?item=" + act.QueueItemID
	}
	if facts := privileged.AgentFacts(act.Type, act.Facts); len(facts) > 0 {
		m["frozen_facts"] = facts
	}
	if len(act.Result) > 0 {
		m["result"] = act.Result
	}
	if act.DecisionNote != nil {
		m["decision_note"] = *act.DecisionNote
	}
	if act.LastError != nil {
		m["last_error"] = *act.LastError
	}
	if act.FinishedAt != nil {
		m["finished_at"] = act.FinishedAt.UTC().Format(time.RFC3339)
	}
	if evs != nil {
		list := make([]map[string]any, 0, len(evs))
		for _, e := range evs {
			list = append(list, map[string]any{"step": e.Step, "detail": e.Detail, "at": e.CreatedAt.UTC().Format(time.RFC3339)})
		}
		m["events"] = list
	}
	return Result{Text: line, Structured: m}
}
