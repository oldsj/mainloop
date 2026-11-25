package privileged

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/redact"
)

// command.omnictl and command.talosctl run one approved omnictl or talosctl command through Omni,
// with the executor's Omni service account (role Admin): the owner's Omni and Talos terminal work
// that no typed action covers.
const (
	TypeOmnictl  = "command.omnictl"
	TypeTalosctl = "command.talosctl"
)

// SideroArgs are command.omnictl's and command.talosctl's arguments. Argv starts with the tool's
// name and runs as is, without a shell. Target is talosctl's cluster, as Omni names it; omnictl
// has none (it acts on Omni). Stdin is what omnictl apply --file=- reads.
type SideroArgs struct {
	Target         string   `json:"target,omitempty"`
	Argv           []string `json:"argv"`
	Stdin          string   `json:"stdin,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

func (a SideroArgs) timeout() time.Duration {
	if a.TimeoutSeconds == 0 {
		return DefaultCommandTimeout
	}
	return time.Duration(a.TimeoutSeconds) * time.Second
}

func sideroTool(typ string) string {
	if typ == TypeTalosctl {
		return ToolTalosctl
	}
	return ToolOmnictl
}

func normalizeSidero(tool string) func(raw json.RawMessage) (any, error) {
	return func(raw json.RawMessage) (any, error) {
		var a SideroArgs
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
		case tool == ToolTalosctl && !dnsLabelRE.MatchString(a.Target):
			return nil, invalidf("target is the cluster's name, as Omni names it")
		case tool == ToolOmnictl && a.Target != "":
			return nil, invalidf("omnictl has no target: it acts on the executor's Omni")
		case len(a.Argv) > MaxArgv || size > MaxArgvBytes:
			return nil, invalidf("argv is at most %d words and %d KiB", MaxArgv, MaxArgvBytes>>10)
		case len(a.Stdin) > MaxStdinBytes:
			return nil, invalidf("stdin is at most %d KiB", MaxStdinBytes>>10)
		case a.TimeoutSeconds < 0 || time.Duration(a.TimeoutSeconds)*time.Second > MaxCommandTimeout:
			return nil, invalidf("timeout_seconds is 1 to %d (default %d)", int(MaxCommandTimeout.Seconds()), int(DefaultCommandTimeout.Seconds()))
		}
		c, err := parseSidero(tool, a.Argv)
		if err != nil {
			return nil, err
		}
		switch {
		case c.spec.stdin && strings.TrimSpace(a.Stdin) == "":
			return nil, invalidf("--file=- reads stdin; give the resources in stdin")
		case !c.spec.stdin && a.Stdin != "":
			return nil, invalidf("stdin is read only by omnictl apply --file=-")
		}
		// The card shows the real command, so a credential in it is refused rather than masked.
		for i, w := range a.Argv {
			if redact.Credential(w) {
				return nil, invalidf("argv[%d] looks like a credential (contains_secret)", i)
			}
		}
		if redact.Credential(a.Stdin) {
			return nil, invalidf("stdin holds what looks like a credential (contains_secret)")
		}
		if a.Stdin != "" {
			if err := checkOmniResources(a.Stdin); err != nil {
				return nil, err
			}
		}
		return a, nil
	}
}

// checkOmniResources reads omnictl apply's resources and refuses a type that may hold a secret.
func checkOmniResources(s string) error {
	dec := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(s), 4096)
	n := 0
	for {
		var r struct {
			Metadata struct {
				Type string `json:"type"`
			} `json:"metadata"`
		}
		if err := dec.Decode(&r); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return invalidf("stdin: %s", err.Error())
		}
		n++
		switch t := r.Metadata.Type; {
		case t == "":
			return invalidf("resource %d of stdin has no metadata.type", n)
		case secretType(t):
			return invalidf("resource %d of stdin is a %s, which may hold secrets: the executor never writes it", n, t)
		case !slices.Contains(omniReadable, t):
			return invalidf("resource %d of stdin is a %s, which is not a type the executor writes (it writes: %s)", n, t, typeNames(ToolOmnictl))
		}
	}
	if n == 0 {
		return invalidf("stdin holds no resource")
	}
	return nil
}

// checkOmniClusters refuses an omnictl command for a cluster the executor does not act on, or
// resources to apply that belong to none of them. The executor's proxy checks every write again.
func checkOmniClusters(c sideroCommand, stdin string, clusters []string) error {
	if (c.path == "cluster status" || c.path == "cluster delete") && !slices.Contains(clusters, c.words[0]) {
		return refuse("cluster %q is not one this executor acts on (%s)", c.words[0], strings.Join(clusters, ", "))
	}
	if stdin == "" {
		return nil
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(stdin), 4096)
	for n := 1; ; n++ {
		var r struct {
			Metadata struct {
				Type   string            `json:"type"`
				ID     string            `json:"id"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		}
		if err := dec.Decode(&r); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return invalidf("stdin: %s", err.Error())
		}
		cluster, ok := omniClusterOf(r.Metadata.Type, r.Metadata.ID, r.Metadata.Labels)
		switch {
		case !ok:
			return refuse("resource %d of stdin (%s %s) belongs to no cluster: label it %s=<cluster>; the executor changes only resources of %s",
				n, r.Metadata.Type, r.Metadata.ID, omniClusterLabel, strings.Join(clusters, ", "))
		case !slices.Contains(clusters, cluster):
			return refuse("resource %d of stdin (%s %s) belongs to cluster %q, which this executor does not act on (%s)",
				n, r.Metadata.Type, r.Metadata.ID, cluster, strings.Join(clusters, ", "))
		}
	}
}

func sideroExpiry(tool string) func(args any) time.Duration {
	return func(args any) time.Duration {
		a, ok := args.(SideroArgs)
		if !ok {
			return UndecidedExpiry
		}
		if c, err := parseSidero(tool, a.Argv); err == nil && c.spec.destructive != "" {
			return DeleteExpiry
		}
		return UndecidedExpiry
	}
}

// SideroFacts are command.omnictl's and command.talosctl's frozen facts.
type SideroFacts struct {
	Tool    string `json:"tool"`
	Cluster string `json:"cluster,omitempty"`
	// Clusters are the clusters an omnictl command is held to.
	Clusters []string `json:"clusters,omitempty"`
	// Preview is PreviewDryRun, PreviewRead or PreviewNone.
	Preview string `json:"preview"`
	// Output is a dry run's text, as the tool prints it.
	Output    string    `json:"output,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// SideroRun is one omnictl or talosctl invocation.
type SideroRun struct {
	Tool string
	// Cluster is talosctl's target: the proxy refuses requests for another.
	Cluster string
	// Argv is the approved argv, the tool's name first.
	Argv    []string
	Stdin   string
	Timeout time.Duration
	// Preflight is the preview before the owner approves: the proxy forwards reads only.
	Preflight bool
	// DryRun adds --dry-run.
	DryRun bool
}

// Sidero runs omnictl and talosctl with the executor's Omni service account.
type Sidero interface {
	Run(ctx context.Context, run SideroRun) (CommandOutput, error)
}

func preflightSidero(tool string) func(ctx context.Context, s *Service, raw json.RawMessage, now time.Time) (any, error) {
	return func(ctx context.Context, s *Service, raw json.RawMessage, now time.Time) (any, error) {
		var a SideroArgs
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		switch {
		case s.Sidero == nil:
			return nil, refuse("this executor has no Omni access")
		case tool == ToolTalosctl && !slices.Contains(s.OmniClusters, a.Target):
			return nil, refuse("target %q is not a cluster this executor may act on (%s)", a.Target, strings.Join(s.OmniClusters, ", "))
		}
		if tool == ToolOmnictl {
			c, err := parseSidero(tool, a.Argv)
			if err != nil {
				return nil, err
			}
			if err := checkOmniClusters(c, a.Stdin, s.OmniClusters); err != nil {
				return nil, err
			}
		}
		f, err := s.sideroPreview(ctx, tool, a)
		if err != nil {
			return nil, err
		}
		if tool == ToolOmnictl {
			f.Clusters = slices.Clone(s.OmniClusters)
		}
		f.CheckedAt = now
		return f, nil
	}
}

// sideroPreview runs a command's dry run, if it has one, through the proxy in read-only mode.
func (s *Service) sideroPreview(ctx context.Context, tool string, a SideroArgs) (SideroFacts, error) {
	c, err := parseSidero(tool, a.Argv)
	if err != nil {
		return SideroFacts{}, err
	}
	f := SideroFacts{Tool: tool, Cluster: a.Target, Preview: c.preview()}
	if f.Preview != PreviewDryRun {
		return f, nil
	}
	out, err := s.Sidero.Run(ctx, SideroRun{Tool: tool, Cluster: a.Target, Argv: a.Argv, Stdin: a.Stdin, Timeout: RemoteTimeout / 2, Preflight: true, DryRun: true})
	if err != nil {
		return f, err
	}
	if out.TimedOut {
		return f, errors.New("the dry run timed out")
	}
	if out.ExitCode != 0 {
		text := cleanOutput(out.Stderr, out.Credentials)
		s.log().Info(tool+" dry run failed", "command", c.path, "exit", out.ExitCode, "stderr", boundText(text, 4<<10))
		return f, refuse("the dry run of %s failed (exit %d%s); the executor's log has its message", c, out.ExitCode, sideroReason(text))
	}
	text := strings.TrimSpace(string(cleanOutput(out.Stdout, out.Credentials)))
	switch {
	case len(text) > MaxPreflightOutput:
		return f, refuse("the dry run's output is over the %d KiB a card shows whole; narrow the command", MaxPreflightOutput>>10)
	case redact.Credential(text):
		return f, refuse("the dry run prints what looks like a credential (contains_secret)")
	}
	f.Output = text
	return f, nil
}

// sideroReason is the gRPC code of the tool's error (", NotFound"), or the proxy's refusal, which
// tell the requester what kind of failure it was and nothing of Omni's state; or "".
func sideroReason(stderr []byte) string {
	s := string(stderr)
	switch {
	case strings.Contains(s, "refused by the executor"):
		return ", refused by the executor's proxy"
	case strings.Contains(s, "code = "):
		code, _, _ := strings.Cut(s[strings.Index(s, "code = ")+len("code = "):], " ")
		if len(code) <= 24 && code != "" && strings.IndexFunc(code, func(r rune) bool { return r < 'A' || r > 'z' }) < 0 {
			return ", " + code
		}
	}
	return ""
}

func renderSidero(r dbq.PrivilegedAction, ctx map[string]any) (string, string, map[string]any, error) {
	var f SideroFacts
	var a SideroArgs
	if json.Unmarshal(r.FrozenFacts, &f) != nil || json.Unmarshal(r.Args, &a) != nil {
		return "", "", nil, fmt.Errorf("privileged: %s facts cannot be read", r.Type)
	}
	c, err := parseSidero(f.Tool, a.Argv)
	if err != nil {
		return "", "", nil, err
	}
	var b strings.Builder
	var title string
	if f.Tool == ToolTalosctl {
		title = fmt.Sprintf("Run talosctl %s on %s (%s) through Omni", c.path, strings.Join(c.nodes, ", "), f.Cluster)
		fmt.Fprintf(&b, "Runs exactly this, once, through Omni as the executor's service account (Omni role Admin), on cluster %s (no shell; the executor sets the endpoint and credential):\n", f.Cluster)
	} else {
		title = "Run omnictl " + c.path + " through Omni"
		b.WriteString("Runs exactly this, once, as the executor's Omni service account (role Admin) (no shell; the executor sets the endpoint and credential):\n")
	}
	b.WriteString(fenced("sh", shellQuote(a.Argv)))
	if f.Tool == ToolTalosctl {
		fmt.Fprintf(&b, "The executor's proxy sends it only to nodes that Omni lists as machines of %s.\n", f.Cluster)
	} else {
		fmt.Fprintf(&b, "Omni may manage other clusters too; the executor's proxy holds this to %s: it changes only their resources, and shows nothing of another cluster's.\n",
			strings.Join(f.Clusters, ", "))
	}
	if a.Stdin != "" {
		b.WriteString("With these resources (--file=- reads them):\n" + fenced("yaml", a.Stdin))
	}
	var tags []string
	switch f.Preview {
	case PreviewDryRun:
		b.WriteString("\nIts dry run, which only reads, says:\n" + fenced("", f.Output))
		b.WriteString("\nJust before it runs, the executor runs the dry run again; if it shows anything else, nothing runs. ")
	case PreviewRead:
		b.WriteString("\nIt only reads. Its output goes back to the requesting session, redacted.\n")
	case PreviewNone:
		b.WriteString("\nNothing can preview it: " + f.Tool + " has no dry run for it.\n")
		tags = append(tags, "no preview")
	}
	b.WriteString("It has no other precondition and no retry: if the answer is lost, the outcome is uncertain and it is not run again.")
	if d := c.spec.destructive; d != "" {
		tags = append([]string{"destructive: " + d}, tags...)
	} else if e := c.effect(); e != "" {
		tags = append([]string{e}, tags...)
	}
	if len(c.nodes) > 1 {
		tags = append(tags, fmt.Sprintf("%d nodes", len(c.nodes)))
	}
	tags = append(tags, "Omni Admin")
	fmt.Fprintf(&b, "\nRisk: %s.", strings.Join(dedupe(tags), ", "))
	if f.Cluster != "" {
		ctx["cluster"] = f.Cluster
	}
	return title, b.String(), ctx, nil
}

// sidero runs command.omnictl or command.talosctl: the dry run again, if it has one, then the
// command once.
func (r *Runner) sidero(ctx context.Context, row dbq.PrivilegedAction) outcome {
	var f SideroFacts
	var a SideroArgs
	if json.Unmarshal(row.FrozenFacts, &f) != nil || json.Unmarshal(row.Args, &a) != nil {
		return outcome{state: StateFailed, err: "the frozen facts cannot be read"}
	}
	if r.Sidero == nil {
		return outcome{state: StateFailed, err: "this executor has no Omni access"}
	}
	tool := sideroTool(row.Type)
	if done, out := r.startedBefore(ctx, row); done {
		return out
	}
	now, err := r.sideroPreview(ctx, tool, a)
	var refused *refusal
	switch {
	case errors.As(err, &refused):
		return outcome{state: StateStale, err: "the dry run now gives something else than the owner saw: " + refused.msg}
	case err != nil:
		return retry(err) // nothing ran yet
	case now.Output != f.Output || now.Preview != f.Preview:
		return outcome{state: StateStale, err: "the dry run now shows a different change than the owner saw; nothing ran"}
	}
	if err := r.extendLease(ctx, row, a.timeout()); err != nil {
		return retry(err)
	}
	if err := r.event(ctx, row.ID, stepStarted, boundText([]byte(shellQuote(a.Argv)), 1<<10)); err != nil {
		return retry(err)
	}
	out, err := r.Sidero.Run(ctx, SideroRun{Tool: tool, Cluster: a.Target, Argv: a.Argv, Stdin: a.Stdin, Timeout: a.timeout()})
	if err != nil {
		return outcome{state: StateUncertain, err: tool + " could not be run to the end: " + safeError(err)}
	}
	return commandOutcome(tool, out, a.timeout())
}

// sideroAgentFacts leaves out the dry run's text, which holds Omni's state read before anyone
// approved.
func sideroAgentFacts(facts json.RawMessage) json.RawMessage {
	var f SideroFacts
	if err := json.Unmarshal(facts, &f); err != nil {
		return nil
	}
	note := ""
	if f.Output != "" {
		note = "the dry run's output is on the owner's card only"
	}
	f.Output = ""
	b, err := json.Marshal(struct {
		SideroFacts
		Note string `json:"note,omitempty"`
	}{f, note})
	if err != nil {
		return nil
	}
	return b
}
