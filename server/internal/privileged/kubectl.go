package privileged

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/redact"
)

// TypeKubectl runs one approved kubectl command as cluster-admin (docs/design/privileged-actions.md
// §1b): the rest of the owner's terminal work on the cluster that no typed action covers.
const TypeKubectl = "command.kubectl"

// AccountAdmin is bound to cluster-admin: command.kubectl. The chart's admission policy keeps it
// (and the narrow accounts) off the executor's own namespace and cluster-wide objects.
const AccountAdmin = "mainloop-executor-admin"

// Bounds of command.kubectl.
const (
	MaxArgv              = 64
	MaxArgvBytes         = 8 << 10
	MaxStdinBytes        = 64 << 10
	MaxCommandOutput     = 64 << 10
	MaxPreflightOutput   = 16 << 10
	DefaultCommandTimeout = 60 * time.Second
	MaxCommandTimeout    = 600 * time.Second
)

// KubectlArgs are command.kubectl's arguments. Argv starts with "kubectl" and runs as is, without a
// shell; Stdin is what `-f -` or `exec -i` reads.
type KubectlArgs struct {
	Target         string   `json:"target"`
	Argv           []string `json:"argv"`
	Stdin          string   `json:"stdin,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

func (a KubectlArgs) timeout() time.Duration {
	if a.TimeoutSeconds == 0 {
		return DefaultCommandTimeout
	}
	return time.Duration(a.TimeoutSeconds) * time.Second
}


func normalizeKubectl(raw json.RawMessage) (any, error) {
	var a KubectlArgs
	if err := decodeStrict(raw, &a); err != nil {
		return nil, err
	}
	a.Target = strings.TrimSpace(a.Target)
	size := 0
	for _, w := range a.Argv {
		size += len(w) + 1
		if !utf8.ValidString(w) || strings.ContainsRune(w, 0) {
			return nil, invalidf("argv words are text without NUL characters")
		}
	}
	switch {
	case a.Target == "":
		return nil, invalidf("target is the cluster's name, as the executor knows it")
	case len(a.Argv) > MaxArgv || size > MaxArgvBytes:
		return nil, invalidf("argv is at most %d words and %d KiB", MaxArgv, MaxArgvBytes>>10)
	case len(a.Stdin) > MaxStdinBytes:
		return nil, invalidf("stdin is at most %d KiB", MaxStdinBytes>>10)
	case a.TimeoutSeconds < 0 || time.Duration(a.TimeoutSeconds)*time.Second > MaxCommandTimeout:
		return nil, invalidf("timeout_seconds is 1 to %d (default %d)", int(MaxCommandTimeout.Seconds()), int(DefaultCommandTimeout.Seconds()))
	}
	c, err := parseKubectl(a.Argv)
	if err != nil {
		return nil, err
	}
	switch {
	case c.stdin && a.Stdin == "" && c.verb != "exec":
		return nil, invalidf("-f - reads stdin; give the manifests in stdin")
	case !c.stdin && a.Stdin != "":
		return nil, invalidf("stdin is read only by -f - or exec -i")
	}
	// The card shows the real command, so a credential in it is refused rather than masked.
	for i, w := range a.Argv {
		if redact.Credential(w) {
			return nil, invalidf("argv[%d] looks like a credential (contains_secret); a secret belongs in a Secret, through onepassword.item_to_secret", i)
		}
	}
	if redact.Credential(a.Stdin) {
		return nil, invalidf("stdin holds what looks like a credential (contains_secret); %s", secretAdvice)
	}
	if c.stdin && c.verb != "exec" {
		objs, err := parseManifests(a.Stdin)
		if err != nil {
			return nil, err
		}
		for i, o := range objs {
			if group, _, _ := strings.Cut(fmt.Sprint(o["apiVersion"]), "/"); group == "admissionregistration.k8s.io" {
				return nil, invalidf("object %d of stdin is an admission policy or webhook: change those through GitOps", i+1)
			}
			if at := requestCredential(o); at != "" {
				return nil, invalidf("object %d of stdin sets what looks like a credential at %s (contains_secret); %s", i+1, at, secretAdvice)
			}
		}
	}
	return a, nil
}

// kubectlExpiry is a delete's for a command that deletes or evicts.
func kubectlExpiry(args any) time.Duration {
	a, ok := args.(KubectlArgs)
	if !ok {
		return UndecidedExpiry
	}
	if c, err := parseKubectl(a.Argv); err == nil && kubectlDestructive(c) != "" {
		return DeleteExpiry
	}
	return UndecidedExpiry
}

// kubectlDestructive names how a command deletes, or "".
func kubectlDestructive(c kubectlCommand) string {
	switch {
	case c.verb == "delete":
		return "deletes objects"
	case c.verb == "drain":
		return "evicts every pod of the node"
	case c.on("force"):
		return "--force deletes and recreates, or skips graceful deletion"
	case c.value("grace-period") == "0":
		return "--grace-period=0 kills without a graceful stop"
	case c.verb == "scale" && c.value("replicas") == "0":
		return "scales to zero"
	}
	return ""
}

// KubectlFacts are command.kubectl's frozen facts.
type KubectlFacts struct {
	Cluster string `json:"cluster"`
	// Preview is how preflight previewed it: PreviewChange, PreviewDryRun, PreviewRead or
	// PreviewNone.
	Preview string `json:"preview"`
	// Changes are the objects a PreviewChange dry run changes, each with its diff; Unchanged those
	// it leaves as they are.
	Changes   []ObjectChange `json:"changes,omitempty"`
	Unchanged []string       `json:"unchanged,omitempty"`
	// Output is a PreviewDryRun's text, as kubectl prints it.
	Output    string    `json:"output,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// preview is what binds the command between approval and the run: re-run before the command,
// it must give the same. A live object's resourceVersion moves with every status update, so it is
// left out: the uid and the diff are what the owner approved.
func (f KubectlFacts) preview() string {
	changes := slices.Clone(f.Changes)
	for i := range changes {
		changes[i].ResourceVersion = ""
	}
	b, _ := json.Marshal(struct {
		Changes   []ObjectChange `json:"c"`
		Unchanged []string       `json:"u"`
		Output    string         `json:"o"`
	}{changes, f.Unchanged, f.Output})
	return string(b)
}

// KubectlRun is one kubectl invocation: the words after "kubectl", stdin and a deadline.
type KubectlRun struct {
	Args    []string
	Stdin   string
	Timeout time.Duration
	// Preflight is the dry run before the owner approves: the executor's proxy makes every write
	// a server dry run and refuses streams, whatever the command asks.
	Preflight bool
}

// KubectlOutput is what one invocation printed. Credentials are the exact values the run held
// (its token), to remove from anything kept.
type KubectlOutput struct {
	ExitCode    int
	Stdout      []byte
	Stderr      []byte
	TimedOut    bool
	Credentials []string
}

// Kubectl runs the kubectl binary as AccountAdmin.
type Kubectl interface {
	Run(ctx context.Context, run KubectlRun) (KubectlOutput, error)
}

func preflightKubectl(ctx context.Context, s *Service, raw json.RawMessage, now time.Time) (any, error) {
	var a KubectlArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if err := s.kubeReady(); err != nil {
		return nil, err
	}
	switch {
	case s.Kubectl == nil:
		return nil, refuse("this executor cannot run kubectl")
	case a.Target != s.Cluster:
		return nil, refuse("target %q is not this executor's cluster (%s)", a.Target, s.Cluster)
	}
	f, err := s.kubectlPreview(ctx, a)
	if err != nil {
		return nil, err
	}
	f.CheckedAt = now
	return f, nil
}

// kubectlPreview previews a command: a server dry run of a change, diffed against the live
// objects, or the dry run's text; nothing for a read or exec.
func (s *Service) kubectlPreview(ctx context.Context, a KubectlArgs) (KubectlFacts, error) {
	c, err := parseKubectl(a.Argv)
	if err != nil {
		return KubectlFacts{}, err
	}
	f := KubectlFacts{Cluster: s.Cluster, Preview: c.preview}
	if own := s.ExecutorNamespace; own != "" && (slices.ContainsFunc(a.Argv, func(w string) bool { return strings.Contains(w, own) }) ||
		strings.Contains(a.Stdin, own)) {
		return f, refuse("the command names %s, the executor's own namespace; it changes only through GitOps the owner merges", own)
	}
	switch c.preview {
	case PreviewChange:
		first, err := s.dryRunObjects(ctx, a, c)
		if err != nil {
			return f, err
		}
		second, err := s.dryRunObjects(ctx, a, c)
		if err != nil {
			return f, err
		}
		return s.kubectlChanges(ctx, f, first, second)
	case PreviewDryRun:
		out, err := s.dryRun(ctx, a, c, nil)
		if err != nil {
			return f, err
		}
		text := strings.TrimSpace(string(out))
		switch {
		case len(text) > MaxPreflightOutput:
			return f, refuse("the dry run's output is over the %d KiB a card shows whole; narrow the command", MaxPreflightOutput>>10)
		case redact.Credential(text):
			return f, refuse("the dry run prints what looks like a credential (contains_secret)")
		case s.ExecutorNamespace != "" && strings.Contains(text, s.ExecutorNamespace):
			return f, refuse("the dry run reaches %s, the executor's own; it changes only through GitOps the owner merges", s.ExecutorNamespace)
		}
		f.Output = text
	}
	return f, nil
}

// dryRun runs the command with --dry-run=server, and extra flags, before any "--".
func (s *Service) dryRun(ctx context.Context, a KubectlArgs, c kubectlCommand, extra []string) ([]byte, error) {
	args := slices.Clone(a.Argv[1:])
	at := slices.Index(args, "--")
	if at < 0 {
		at = len(args)
	}
	args = slices.Insert(args, at, append([]string{"--dry-run=server"}, extra...)...)
	out, err := s.Kubectl.Run(ctx, KubectlRun{Args: args, Stdin: a.Stdin, Timeout: RemoteTimeout / 2, Preflight: true})
	if err != nil {
		return nil, err
	}
	if out.TimedOut {
		return nil, errors.New("the dry run timed out")
	}
	if out.ExitCode != 0 {
		// What kubectl printed is cluster state read before anyone approved: the requester gets
		// its reason only, and the executor's log keeps the text.
		text := cleanOutput(out.Stderr, out.Credentials)
		s.log().Info("kubectl dry run failed", "command", c.path, "exit", out.ExitCode, "stderr", boundText(text, 4<<10))
		return nil, refuse("the server dry run of kubectl %s failed (exit %d%s); the executor's log has kubectl's message", c.path, out.ExitCode, errorReason(text))
	}
	return cleanOutput(out.Stdout, out.Credentials), nil
}

var reasonRE = regexp.MustCompile(`Error from server \(([A-Za-z]{1,40})\)`)

// errorReason is the Kubernetes reason of kubectl's first error (", NotFound"), which tells the
// requester what kind of failure it was and nothing of the cluster, or "".
func errorReason(stderr []byte) string {
	switch m := reasonRE.FindSubmatch(stderr); {
	case m != nil:
		return ", " + string(m[1])
	case bytes.Contains(stderr, []byte("refused by the executor")):
		return ", refused by the executor's proxy"
	}
	return ""
}

// dryRunObjects runs a change's server dry run with -o json and reads the objects it prints.
func (s *Service) dryRunObjects(ctx context.Context, a KubectlArgs, c kubectlCommand) ([]map[string]any, error) {
	out, err := s.dryRun(ctx, a, c, []string{"-o", "json"})
	if err != nil {
		return nil, err
	}
	var objs []map[string]any
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var o map[string]any
		if err := dec.Decode(&o); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, refuse("the dry run of kubectl %s printed no objects to preview; it cannot go to the owner as a command", c.verb)
		}
		if items, ok := o["items"].([]any); ok && strings.HasSuffix(fmt.Sprint(o["kind"]), "List") {
			for _, it := range items {
				if m, ok := it.(map[string]any); ok {
					objs = append(objs, m)
				}
			}
			continue
		}
		objs = append(objs, o)
	}
	if len(objs) == 0 {
		return nil, refuse("the dry run of kubectl %s changes no object", c.verb)
	}
	if len(objs) > MaxApplyObjects {
		return nil, refuse("the command changes over %d objects; narrow it", MaxApplyObjects)
	}
	return objs, nil
}

// kubectlChanges diffs each dry-run object against the live one, masking what two dry runs
// disagree on, as k8s.apply does.
func (s *Service) kubectlChanges(ctx context.Context, f KubectlFacts, first, second []map[string]any) (KubectlFacts, error) {
	seconds := map[string]map[string]any{}
	for _, o := range second {
		seconds[objectKey(o)] = o
	}
	for _, o := range first {
		u := &unstructured.Unstructured{Object: o}
		r, err := s.Kube.ResolveObject(ctx, u.GetAPIVersion(), u.GetKind())
		if err != nil {
			return f, badTarget(err)
		}
		ns, name := u.GetNamespace(), u.GetName()
		target := objectName(r, ns, name)
		switch {
		case isSecret(r):
			return f, refuse("%s: commands that change a Secret are refused: the dry run would print its value. Use onepassword.item_to_secret", target)
		case s.selfTarget(ns, r.Kind, name, r.Namespaced):
			return f, refuse("%s is the executor's own; it changes only through GitOps", target)
		case r.Group == "admissionregistration.k8s.io":
			return f, refuse("%s is an admission policy or webhook; those change only through GitOps", target)
		}
		live, err := s.Kube.Get(ctx, r, ns, name)
		if err != nil {
			return f, badTarget(err)
		}
		other, ok := seconds[objectKey(o)]
		if !ok {
			return f, refuse("two dry runs of the command changed different objects; it cannot be previewed")
		}
		var gen []string
		generatedPaths(comparable(o), comparable(other), "", &gen)
		after := maskGenerated(o, gen)
		diff, err := objectDiff(live, after)
		if err != nil {
			return f, err
		}
		if diff == "" {
			f.Unchanged = append(f.Unchanged, target)
			continue
		}
		if at := credentialAt(diff, comparable(live), comparable(after)); at != "" {
			return f, refuse("the change to %s shows what looks like a credential at %s (contains_secret); %s", target, at, secretAdvice)
		}
		if redact.Credential(diff) {
			return f, refuse("the change to %s shows what looks like a credential in its diff (contains_secret); %s", target, secretAdvice)
		}
		ch := newChange(r, ns, name, live, diff)
		ch.Generated = gen
		f.Changes = append(f.Changes, ch)
	}
	if len(f.Changes) == 0 {
		return f, refuse("the command changes nothing: the dry run leaves every object as it is")
	}
	if err := (ChangeFacts{Changes: f.Changes}).check(); err != nil {
		return f, err
	}
	return f, nil
}

func objectKey(o map[string]any) string {
	u := &unstructured.Unstructured{Object: o}
	return u.GetAPIVersion() + "/" + u.GetKind() + "/" + u.GetNamespace() + "/" + u.GetName()
}

// cleanOutput applies the session redactor, then removes every exact credential the run held, in
// plain and base64 form, that it left.
func cleanOutput(b []byte, credentials []string) []byte {
	s := redact.Text(string(b))
	for _, c := range credentials {
		if c == "" {
			continue
		}
		s = strings.ReplaceAll(s, c, "[redacted]")
		s = strings.ReplaceAll(s, base64.StdEncoding.EncodeToString([]byte(c)), "[redacted]")
	}
	return []byte(s)
}

// boundText cuts s to n bytes on a character boundary, saying so.
func boundText(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(b[cut]) {
		cut--
	}
	return string(b[:cut]) + fmt.Sprintf("\n[… %d more bytes not kept]", len(b)-cut)
}

const shellSafe = "-_./:=,@abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// shellQuote shows argv as one would type it.
func shellQuote(argv []string) string {
	out := make([]string, 0, len(argv))
	for _, w := range argv {
		if w != "" && strings.IndexFunc(w, func(r rune) bool { return !strings.ContainsRune(shellSafe, r) }) < 0 {
			out = append(out, w)
			continue
		}
		out = append(out, "'"+strings.ReplaceAll(w, "'", `'\''`)+"'")
	}
	return strings.Join(out, " ")
}

// fenced puts text in a code block whose fence is longer than any run of backticks in it, so
// nothing in the text can end the block early.
func fenced(lang, text string) string {
	longest, run := 0, 0
	for _, ch := range text {
		if ch == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + lang + "\n" + strings.TrimSuffix(text, "\n") + "\n" + fence + "\n"
}

func renderKubectl(r dbq.PrivilegedAction, ctx map[string]any) (string, string, map[string]any, error) {
	var f KubectlFacts
	var a KubectlArgs
	if json.Unmarshal(r.FrozenFacts, &f) != nil || json.Unmarshal(r.Args, &a) != nil {
		return "", "", nil, errors.New("privileged: command.kubectl facts cannot be read")
	}
	c, err := parseKubectl(a.Argv)
	if err != nil {
		return "", "", nil, err
	}
	verb := c.path
	title := "Run kubectl " + verb + onCluster(f.Cluster) + " as cluster-admin"
	var b strings.Builder
	fmt.Fprintf(&b, "Runs exactly this, once, as cluster-admin on %s (no shell; the executor sets the target and credential):\n", f.Cluster)
	b.WriteString(fenced("sh", shellQuote(a.Argv)))
	if a.Stdin != "" {
		b.WriteString("With this on stdin:\n" + fenced("yaml", a.Stdin))
	}
	var tags []string
	switch f.Preview {
	case PreviewChange:
		b.WriteString("\nThe API server's dry run of it, against the live objects:\n")
		for _, ch := range f.Changes {
			if ch.UID == "" {
				fmt.Fprintf(&b, "\n%s (new):\n", ch.target())
			} else {
				fmt.Fprintf(&b, "\n%s (uid %s):\n", ch.target(), ch.UID)
			}
			b.WriteString(fenced("diff", ch.Diff))
			if len(ch.Generated) > 0 {
				fmt.Fprintf(&b, "%s marks values the API server generates on each run (%s).\n", GeneratedValue, strings.Join(ch.Generated, ", "))
			}
			tags = append(tags, changeTags(ch)...)
		}
		if len(f.Unchanged) > 0 {
			fmt.Fprintf(&b, "\nLeft as they are: %s.\n", strings.Join(f.Unchanged, ", "))
		}
	case PreviewDryRun:
		b.WriteString("\nThe API server's dry run of it says:\n" + fenced("", f.Output))
	case PreviewRead:
		b.WriteString("\nIt only reads. Its output goes back to the requesting session, redacted.\n")
	case PreviewNone:
		b.WriteString("\nNothing can preview it: it runs a command inside a container, which can do anything that container can.\n")
		tags = append(tags, "runs inside a container")
	}
	if f.Preview == PreviewChange || f.Preview == PreviewDryRun {
		b.WriteString("\nJust before it runs, the executor runs the dry run again; if it shows anything else, nothing runs. ")
	}
	b.WriteString("Unlike a typed action it has no other precondition and no retry: if the answer is lost, the outcome is uncertain and it is not run again.")
	if d := kubectlDestructive(c); d != "" {
		tags = append([]string{"destructive: " + d}, tags...)
	}
	for _, ns := range c.flags["namespace"] {
		tags = append(tags, namespaceTags(ns)...)
	}
	if c.on("all-namespaces") {
		tags = append(tags, "every namespace")
	}
	tags = append(tags, "cluster-admin")
	if tags = dedupe(tags); len(tags) > 0 {
		fmt.Fprintf(&b, "\nRisk: %s.", strings.Join(tags, ", "))
	}
	if ns := c.value("namespace"); ns != "" {
		ctx["namespace"] = ns
	}
	return title, b.String(), ctx, nil
}

// KubectlResult is command.kubectl's result: what kubectl printed, without the credential it held,
// through the session redactor and bounded.
type KubectlResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
}

// stepStarted is the audit event written just before the command runs: an attempt that finds it
// knows an earlier one may have run the command, and does not run it again.
const stepStarted = "command_started"

// kubectl runs command.kubectl: the dry run again, then the command once.
func (r *Runner) kubectl(ctx context.Context, row dbq.PrivilegedAction) outcome {
	var f KubectlFacts
	var a KubectlArgs
	if json.Unmarshal(row.FrozenFacts, &f) != nil || json.Unmarshal(row.Args, &a) != nil {
		return outcome{state: StateFailed, err: "the frozen facts cannot be read"}
	}
	if r.Kube == nil || r.Kubectl == nil {
		return outcome{state: StateFailed, err: "this executor cannot run kubectl"}
	}
	evs, err := dbq.New(r.Pool).ListPrivilegedActionEvents(ctx, row.ID)
	if err != nil {
		return retry(err)
	}
	if slices.ContainsFunc(evs, func(e dbq.PrivilegedActionEvent) bool { return e.Step == stepStarted }) {
		return outcome{state: StateUncertain, err: "an earlier attempt started the command and its outcome was lost; it is not run again"}
	}
	now, err := r.kubectlPreview(ctx, a)
	var refused *refusal
	switch {
	case errors.As(err, &refused):
		return outcome{state: StateStale, err: "the dry run now gives something else than the owner saw: " + refused.msg}
	case err != nil:
		return retry(err) // nothing ran yet
	case now.preview() != f.preview():
		return outcome{state: StateStale, err: "the dry run now shows a different change than the owner saw; nothing ran"}
	}
	if err := r.extendLease(ctx, row, a.timeout()); err != nil {
		return retry(err)
	}
	if err := r.event(ctx, row.ID, stepStarted, "kubectl "+boundText([]byte(shellQuote(a.Argv[1:])), 1<<10)); err != nil {
		return retry(err)
	}
	out, err := r.Kubectl.Run(ctx, KubectlRun{Args: a.Argv[1:], Stdin: a.Stdin, Timeout: a.timeout()})
	if err != nil {
		return outcome{state: StateUncertain, err: "kubectl could not be run to the end: " + safeError(err)}
	}
	res := KubectlResult{ExitCode: out.ExitCode}
	stdout := cleanOutput(out.Stdout, out.Credentials)
	stderr := cleanOutput(out.Stderr, out.Credentials)
	res.Stdout = boundText(stdout, MaxCommandOutput)
	if room := MaxCommandOutput - min(len(stdout), MaxCommandOutput); room > 0 {
		res.Stderr = boundText(stderr, max(room, 4<<10))
	} else {
		res.Stderr = boundText(stderr, 4<<10)
	}
	switch {
	case out.TimedOut:
		return outcome{state: StateUncertain, result: res, err: fmt.Sprintf("kubectl ran past its %s and was stopped; what it changed is unknown", a.timeout())}
	case out.ExitCode != 0:
		return outcome{state: StateFailed, result: res, err: fmt.Sprintf("kubectl exited %d", out.ExitCode)}
	}
	return outcome{state: StateSucceeded, result: res}
}

// extendLease keeps the claim past the command's deadline, so no other attempt starts meanwhile.
func (r *Runner) extendLease(ctx context.Context, row dbq.PrivilegedAction, d time.Duration) error {
	n, err := dbq.New(r.Pool).ExtendPrivilegedLease(ctx, dbq.ExtendPrivilegedLeaseParams{
		ID: row.ID, Attempts: row.Attempts, LeaseSeconds: (d + RemoteTimeout).Seconds(),
	})
	if err == nil && n == 0 {
		err = errors.New("the claim was lost")
	}
	return err
}

// kubectlAgentFacts leaves out the diffs and dry-run text, which hold the objects' contents read
// with cluster-admin before anyone approved.
func kubectlAgentFacts(facts json.RawMessage) json.RawMessage {
	var f KubectlFacts
	if err := json.Unmarshal(facts, &f); err != nil {
		return nil
	}
	out := struct {
		Cluster   string    `json:"cluster"`
		Preview   string    `json:"preview"`
		Changes   []string  `json:"changes,omitempty"`
		Unchanged []string  `json:"unchanged,omitempty"`
		Note      string    `json:"note"`
		CheckedAt time.Time `json:"checked_at"`
	}{Cluster: f.Cluster, Preview: f.Preview, Unchanged: f.Unchanged, CheckedAt: f.CheckedAt, Note: "the preview is on the owner's card only"}
	for _, c := range f.Changes {
		out.Changes = append(out.Changes, c.target())
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return b
}
