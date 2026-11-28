package privileged

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/redact"
)

// command.exec runs one approved script in a fresh, single-use pod of the toolbox image
// (images/toolbox): the owner's everyday CLIs at pinned versions, with no credential of the
// executor's. It is the escape hatch for what no typed action covers: a CLI that is not kubectl, a
// script that chains tools, a download or a check from outside. The pod is the boundary, so the
// script is a bash script, not a command line.
const TypeExec = "command.exec"

// The toolbox's single target and the reaches a run can have. internet is the only reach so far.
const (
	ToolboxTarget = "toolbox"
	ReachInternet = "internet"
)

// MaxScriptBytes bounds a command.exec script.
const MaxScriptBytes = 64 << 10

// toolboxPendingLimit is how long a run's pod may wait to start (scheduling and the image's pull,
// large on a node's first run) before the run fails with nothing having run.
const toolboxPendingLimit = 8 * time.Minute

// ExecArgs are command.exec's arguments. Script runs as `bash -euo pipefail`; Stdin is what it
// reads. Both are shown in full on the card.
type ExecArgs struct {
	Target         string `json:"target"`
	Script         string `json:"script"`
	Stdin          string `json:"stdin,omitempty"`
	Reach          string `json:"reach"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

func (a ExecArgs) timeout() time.Duration {
	if a.TimeoutSeconds == 0 {
		return DefaultCommandTimeout
	}
	return time.Duration(a.TimeoutSeconds) * time.Second
}

func normalizeExec(raw json.RawMessage) (any, error) {
	var a ExecArgs
	if err := decodeStrict(raw, &a); err != nil {
		return nil, err
	}
	a.Target, a.Reach = strings.TrimSpace(a.Target), strings.TrimSpace(a.Reach)
	switch {
	case a.Target != ToolboxTarget:
		return nil, invalidf("target is %q, the only target", ToolboxTarget)
	case a.Reach == "cluster":
		return nil, invalidf("reach %q is not available yet; reach is %q", a.Reach, ReachInternet)
	case a.Reach != ReachInternet:
		return nil, invalidf("reach is %q: the run reaches the internet and nothing inside the network", ReachInternet)
	case strings.TrimSpace(a.Script) == "":
		return nil, invalidf("script is the bash script the toolbox runs")
	case len(a.Script) > MaxScriptBytes:
		return nil, invalidf("script is at most %d KiB", MaxScriptBytes>>10)
	case !utf8.ValidString(a.Script) || strings.ContainsFunc(a.Script, unprintableText):
		return nil, invalidf("script is text: no control characters but newlines and tabs, and no invisible ones")
	case bareCR(a.Script):
		return nil, invalidf("script has a carriage return that does not end a line (\\r without \\n)")
	case len(a.Stdin) > MaxStdinBytes:
		return nil, invalidf("stdin is at most %d KiB", MaxStdinBytes>>10)
	case !utf8.ValidString(a.Stdin) || strings.ContainsFunc(a.Stdin, unprintableText):
		return nil, invalidf("stdin is text: no control characters but newlines and tabs, and no invisible ones")
	case bareCR(a.Stdin):
		return nil, invalidf("stdin has a carriage return that does not end a line (\\r without \\n)")
	case a.TimeoutSeconds < 0 || time.Duration(a.TimeoutSeconds)*time.Second > MaxCommandTimeout:
		return nil, invalidf("timeout_seconds is 1 to %d (default %d)", int(MaxCommandTimeout.Seconds()), int(DefaultCommandTimeout.Seconds()))
	}
	// The card shows the real script, so a credential in it is refused rather than masked.
	if redact.Credential(a.Script) {
		return nil, invalidf("script looks like it holds a credential (contains_secret)")
	}
	if redact.Credential(a.Stdin) {
		return nil, invalidf("stdin holds what looks like a credential (contains_secret)")
	}
	return a, nil
}

// bareCR reports a carriage return that does not end a line: it can make a line show as something
// other than what runs.
func bareCR(s string) bool { return strings.Contains(strings.ReplaceAll(s, "\r\n", ""), "\r") }

// execDestructiveWords and execInstallWords mark a script that may delete or wipe, or install
// software: a word scan for the card's tags and a delete's expiry, not a guard (a script can spell
// anything another way).
var (
	execDestructiveWords = []string{"rm", "rmdir", "shred", "unlink", "dd", "mkfs", "wipefs", "truncate", "delete", "destroy",
		"drop", "purge", "prune", "wipe", "erase", "uninstall", "revoke", "terminate"}
	execInstallWords = []string{"install", "apt", "apt-get", "dpkg", "pip", "pip3", "pipx", "npm", "npx", "pnpm", "uvx", "cargo", "gem"}
)

func execMatches(a ExecArgs, list []string) []string {
	var out []string
	for _, w := range sshWords(SSHArgs{Command: a.Script, Stdin: a.Stdin}) {
		if slices.Contains(list, w) && !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

func execExpiry(args any) time.Duration {
	if a, ok := args.(ExecArgs); ok && len(execMatches(a, execDestructiveWords)) > 0 {
		return DeleteExpiry
	}
	return UndecidedExpiry
}

// ExecFacts are command.exec's frozen facts: the image the run will use, by digest, and its reach.
// There is no preview: nothing runs until the owner approves.
type ExecFacts struct {
	Image     string    `json:"image"`
	Reach     string    `json:"reach"`
	CheckedAt time.Time `json:"checked_at"`
}

// ToolboxPhase is where a run's pod is.
type ToolboxPhase string

// The phases a run's pod passes through. Pending means no container has started: nothing ran.
const (
	ToolboxPending ToolboxPhase = "pending"
	ToolboxRunning ToolboxPhase = "running"
	ToolboxExited  ToolboxPhase = "exited"
	// ToolboxFailed is a pod that ended without its container reporting an exit: evicted, or past
	// its deadline.
	ToolboxFailed ToolboxPhase = "failed"
)

// ToolboxSpec is one run: everything the pod is made from.
type ToolboxSpec struct {
	ActionID string
	Image    string
	Reach    string
	Script   string
	Stdin    string
	Timeout  time.Duration
}

// ToolboxState is what the executor can observe of a run's pod.
type ToolboxState struct {
	Exists bool
	Phase  ToolboxPhase
	// Started is set once the container has run: from then on the script may have acted.
	Started  bool
	ExitCode int
	// Reason is the container's or pod's reason: Completed, Error, OOMKilled, DeadlineExceeded,
	// Evicted, or the wait that holds a pending pod (ImagePullBackOff).
	Reason string
	// Message is the pod's own message about a wait; it never holds the script's output.
	Message string
}

// ToolboxRef names a run's pod for the reaper.
type ToolboxRef struct {
	ActionID  string
	CreatedAt time.Time
}

// Toolbox creates, observes and removes the toolbox pods of command.exec. A pod's name is derived
// from the action's id, so creating it again after a crash finds the same run.
type Toolbox interface {
	// Ready checks that the executor can make runs, without making one. A *refusal says why not.
	Ready(ctx context.Context) error
	// Create makes the run's pod. A pod that already exists for the action is the same run, not an
	// error. An error that is a *NothingRan means the API server refused it.
	Create(ctx context.Context, spec ToolboxSpec) error
	// State reads the run's pod.
	State(ctx context.Context, actionID string) (ToolboxState, error)
	// Output reads what the run printed, stdout and stderr together, at most max bytes.
	Output(ctx context.Context, actionID string, max int) ([]byte, error)
	// Delete removes the run's pod and its Secrets; a missing one is not an error.
	Delete(ctx context.Context, actionID string) error
	// Runs lists the pods that exist.
	Runs(ctx context.Context) ([]ToolboxRef, error)
}

func preflightExec(ctx context.Context, s *Service, raw json.RawMessage, now time.Time) (any, error) {
	var a ExecArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	switch {
	case s.Toolbox == nil || s.ToolboxImage == "":
		return nil, refuse("this executor has no toolbox")
	case a.Reach != ReachInternet:
		return nil, refuse("reach %q is not available on this executor", a.Reach)
	}
	if err := s.Toolbox.Ready(ctx); err != nil {
		return nil, err
	}
	return ExecFacts{Image: s.ToolboxImage, Reach: a.Reach, CheckedAt: now}, nil
}

// imageDigestRE is a reference pinned by digest, as the executor's configuration must give it.
var imageDigestRE = regexp.MustCompile(`^[a-z0-9][a-z0-9./_:-]*@sha256:[a-f0-9]{64}$`)

// ValidToolboxImage reports whether ref is an image reference pinned by digest.
func ValidToolboxImage(ref string) bool { return len(ref) <= 300 && imageDigestRE.MatchString(ref) }

// shortDigest is an image reference's name and the first 12 hex digits of its digest.
func shortDigest(ref string) string {
	name, digest, ok := strings.Cut(ref, "@sha256:")
	if !ok {
		return ref
	}
	return name + "@sha256:" + digest[:min(12, len(digest))]
}

func renderExec(r dbq.PrivilegedAction, ctx map[string]any) (string, string, map[string]any, error) {
	var f ExecFacts
	var a ExecArgs
	if json.Unmarshal(r.FrozenFacts, &f) != nil || json.Unmarshal(r.Args, &a) != nil {
		return "", "", nil, fmt.Errorf("privileged: %s facts cannot be read", r.Type)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Runs this script once, as bash -euo pipefail, in a fresh single-use pod of the toolbox image (%s), "+
		"for at most %s. The pod holds no credential of Mainloop's:\n", shortDigest(f.Image), a.timeout())
	b.WriteString(fenced("sh", a.Script))
	if a.Stdin != "" {
		b.WriteString("With this on its stdin:\n" + fenced("", a.Stdin))
	}
	b.WriteString("\nIt can reach the internet (DNS and public addresses) and nothing inside the network: not the cluster, not the " +
		"home network. ")
	b.WriteString("Nothing can preview it: nothing has run. ")
	b.WriteString("There is no retry: if the outcome is lost, it is uncertain and the script is not run again. ")
	b.WriteString("Its output (exit code, stdout and stderr together, redacted and bounded) goes back to the agent and is stored with the action.")
	tags := []string{"toolbox", "reach: " + f.Reach}
	if w := execMatches(a, execDestructiveWords); len(w) > 0 {
		tags = append([]string{"destructive: may delete or wipe (" + strings.Join(w, ", ") + ")"}, tags...)
	}
	if w := execMatches(a, execInstallWords); len(w) > 0 {
		tags = append(tags, "may install software ("+strings.Join(w, ", ")+")")
	}
	tags = append(tags, "no preview")
	fmt.Fprintf(&b, "\nRisk: %s. (The destructive and install tags come from a word scan, not a guarantee.)", strings.Join(tags, ", "))
	ctx["reach"] = f.Reach
	return "Run a script in the toolbox", b.String(), ctx, nil
}

// execPodNames are the pod's name and its Secret's, for an action id: ids hold an underscore,
// which names do not.
func execPodName(actionID string) string {
	return "exec-" + strings.ReplaceAll(strings.ToLower(actionID), "_", "-")
}

// exec runs command.exec: the pod by name first (an earlier attempt may have made it), then the
// pod once, then its outcome. The pod is deleted later by the reaper, once the action is final, so
// a lost answer can be read again.
func (r *Runner) exec(ctx context.Context, row dbq.PrivilegedAction) outcome {
	var f ExecFacts
	var a ExecArgs
	if json.Unmarshal(row.FrozenFacts, &f) != nil || json.Unmarshal(row.Args, &a) != nil {
		return outcome{state: StateFailed, err: "the frozen facts cannot be read"}
	}
	if r.Toolbox == nil || r.ToolboxImage == "" {
		return outcome{state: StateFailed, err: "this executor no longer has a toolbox"}
	}
	st, err := r.Toolbox.State(ctx, row.ID)
	if err != nil {
		return retry(err) // nothing known to have run yet
	}
	if !st.Exists {
		if done, out := r.startedBefore(ctx, row); done {
			return out
		}
		if f.Image != r.ToolboxImage || f.Reach != a.Reach {
			return outcome{state: StateStale, err: "the toolbox image changed since the owner saw it (" + shortDigest(f.Image) +
				", now " + shortDigest(r.ToolboxImage) + "); nothing ran"}
		}
		if err := r.extendLease(ctx, row, a.timeout()+toolboxPendingLimit); err != nil {
			return retry(err)
		}
		if err := r.event(ctx, row.ID, stepStarted, "toolbox "+shortDigest(f.Image)); err != nil {
			return retry(err)
		}
		err := r.Toolbox.Create(ctx, ToolboxSpec{
			ActionID: row.ID, Image: f.Image, Reach: a.Reach, Script: a.Script, Stdin: a.Stdin, Timeout: a.timeout(),
		})
		var before *NothingRan
		switch {
		case errors.As(err, &before):
			// Final, not retried: the started event is written, and a retry must never guess.
			return outcome{state: StateFailed, err: "nothing ran: " + safeError(before.Err)}
		case err != nil:
			return retry(err) // the next attempt finds the pod, or the started event
		}
	} else if err := r.extendLease(ctx, row, a.timeout()+toolboxPendingLimit); err != nil {
		return retry(err) // an earlier attempt's pod: keep the claim while it is followed
	}
	return r.awaitExec(ctx, row, a)
}

// awaitExec follows the run's pod to its end.
func (r *Runner) awaitExec(ctx context.Context, row dbq.PrivilegedAction, a ExecArgs) outcome {
	poll := r.ToolboxPoll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	waitStart := r.now()
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		st, err := r.Toolbox.State(ctx, row.ID)
		switch {
		case err != nil && ctx.Err() == nil:
			return retry(err)
		case err != nil:
			return outcome{err: safeError(ctx.Err())}
		case !st.Exists:
			return outcome{state: StateUncertain, err: "the run's pod disappeared before its outcome was read; it is not run again"}
		}
		switch st.Phase {
		case ToolboxPending:
			if r.now().Sub(waitStart) > toolboxPendingLimit {
				_ = r.Toolbox.Delete(ctx, row.ID)
				why := st.Reason
				if why == "" {
					why = "not scheduled"
				}
				return outcome{state: StateFailed, err: fmt.Sprintf("the toolbox pod did not start within %s (%s); nothing ran", toolboxPendingLimit, why)}
			}
		case ToolboxRunning:
			// The container's own timeout and the pod's deadline end it; the context bounds this wait.
		case ToolboxExited, ToolboxFailed:
			return r.execOutcome(ctx, row, a, st)
		}
		select {
		case <-ctx.Done():
			return outcome{err: safeError(ctx.Err())} // the lease lets the next attempt read it
		case <-t.C:
		}
	}
}

// execOutcome is the run's outcome from its pod's end state.
func (r *Runner) execOutcome(ctx context.Context, row dbq.PrivilegedAction, a ExecArgs, st ToolboxState) outcome {
	if st.Phase == ToolboxFailed && !st.Started {
		return outcome{state: StateFailed, err: "the toolbox pod ended before it ran (" + st.Reason + "); nothing ran"}
	}
	var out CommandOutput
	b, err := r.Toolbox.Output(ctx, row.ID, maxCaptured)
	if err != nil {
		// The pod is still there: the next attempt reads it again.
		return retry(err)
	}
	out.Stdout = b
	switch {
	case st.Reason == "DeadlineExceeded", st.Phase == ToolboxExited && (st.ExitCode == 124 || st.ExitCode == 137) && st.Reason != "OOMKilled":
		out.TimedOut = true
	case st.Phase == ToolboxFailed:
		o := commandOutcome("the script", out, a.timeout())
		return outcome{state: StateUncertain, result: o.result, err: "the toolbox pod ended abnormally (" + st.Reason + "); the script may have acted"}
	default:
		out.ExitCode = st.ExitCode
	}
	o := commandOutcome("the script", out, a.timeout())
	if st.Reason == "OOMKilled" {
		o.err = "the script was killed for using more than the toolbox's memory limit"
	}
	return o
}

// toolboxReapEvery is how often the executor looks for pods of finished actions.
const toolboxReapEvery = 30 * time.Second

// reapToolbox deletes the pods (and their Secrets) of actions that are final, or that no row
// knows. A pod outlives its run so that a lost answer can be read again; it goes once the outcome
// is stored. A pod whose action still executes is left alone: its own deadline ends it.
func (r *Runner) reapToolbox(ctx context.Context) error {
	every := r.ToolboxReap
	if every == 0 {
		every = toolboxReapEvery
	}
	if r.Toolbox == nil || every > 0 && r.now().Sub(r.reapedAt) < every {
		return nil
	}
	r.reapedAt = r.now()
	refs, err := r.Toolbox.Runs(ctx)
	if err != nil {
		return fmt.Errorf("list toolbox pods: %w", err)
	}
	q := dbq.New(r.Pool)
	for _, ref := range refs {
		row, err := q.GetPrivilegedAction(ctx, dbq.GetPrivilegedActionParams{OwnerID: r.OwnerID, ID: ref.ActionID})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// No such action: a pod of another release, or a test. Leave a young one to its owner.
			if r.now().Sub(ref.CreatedAt) < time.Hour {
				continue
			}
		case err != nil:
			return err
		case row.State == StateExecuting || row.State == StateApproved:
			continue
		}
		if err := r.Toolbox.Delete(ctx, ref.ActionID); err != nil {
			return fmt.Errorf("delete toolbox pod: %w", err)
		}
	}
	return nil
}
