package privileged

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/spf13/pflag"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	kubectlcmd "k8s.io/kubectl/pkg/cmd"
)

// KubectlVersion is the kubectl the prod image pins, and the k8s.io/kubectl module argv is parsed
// with: the two must be the same release, so the parser reads a command exactly as the binary does.
const KubectlVersion = "v1.37.2"

// Preview kinds: what preflight shows of a command before it runs.
const (
	// PreviewChange is a server dry run whose objects are diffed against the live ones.
	PreviewChange = "change"
	// PreviewDryRun is a server dry run whose text is shown as kubectl prints it.
	PreviewDryRun = "dry_run"
	// PreviewRead is a command that only reads: it has nothing to preview.
	PreviewRead = "read"
	// PreviewNone is a command nothing can preview (exec runs inside a container).
	PreviewNone = "none"
)

// kubectlSpec is one allowed command: its preview and the flags it may be given. Every other
// command (edit, cp, proxy, port-forward, attach, debug, config, plugin, kustomize, certificate,
// create secret, create token, cluster-info dump, a plugin) and every other flag is refused: an
// allowlist, so a flag kubectl adds later is refused until it is read and added here.
type kubectlSpec struct {
	preview string
	flags   []string
}

// globalFlags are allowed on every command. kubectl's other global flags choose the target, the
// identity or the credential (the executor sets those), or write local files.
var globalFlags = []string{"namespace", "request-timeout"}

// refusedWhy says why a flag no command allows is refused, for the flags someone is likely to try.
// The decision is the allowlist; this is only the message.
var refusedWhy = map[string]string{
	"kubeconfig": "the executor sets the target", "context": "the executor sets the target", "cluster": "the executor sets the target",
	"server": "the executor sets the target", "certificate-authority": "the executor sets the target",
	"insecure-skip-tls-verify": "the executor sets the target", "tls-server-name": "the executor sets the target",
	"user": "the executor sets the credential", "token": "the executor sets the credential", "username": "the executor sets the credential",
	"password": "the executor sets the credential", "client-certificate": "the executor sets the credential",
	"client-key": "the executor sets the credential",
	"as": "no impersonation", "as-group": "no impersonation", "as-uid": "no impersonation", "as-user-extra": "no impersonation",
	"raw": "it reads or writes any API path, Secrets included", "kustomize": "it reads a local directory",
	"recursive": "it reads local directories", "dry-run": "the executor runs the dry run itself",
	"prune": "it deletes objects the card cannot show; use argocd.sync with prune, or delete them",
	"watch": "it never ends", "watch-only": "it never ends", "tty": "there is no terminal",
	"all-namespaces": "name the objects, or select them with -l in one namespace",
	"all":            "name the objects, or select them with -l in one namespace",
}

var (
	printFlags  = []string{"output", "template", "allow-missing-template-keys", "show-managed-fields"}
	changeFlags = append([]string{"field-manager", "validate"}, printFlags...)
)

func flags(groups ...[]string) []string { return slices.Concat(groups...) }

var kubectlCommands = map[string]kubectlSpec{
	"get": {PreviewRead, flags(printFlags, []string{"all-namespaces", "selector", "field-selector", "show-labels", "label-columns",
		"sort-by", "show-kind", "no-headers", "ignore-not-found", "chunk-size", "subresource", "server-print"})},
	"describe":       {PreviewRead, []string{"all-namespaces", "selector", "show-events", "chunk-size"}},
	"logs":           {PreviewRead, []string{"container", "all-containers", "previous", "since", "since-time", "tail", "timestamps", "selector", "max-log-requests", "prefix", "limit-bytes", "ignore-errors", "pod-running-timeout"}},
	"top pod":        {PreviewRead, []string{"all-namespaces", "selector", "field-selector", "containers", "sort-by", "no-headers", "sum"}},
	"top node":       {PreviewRead, []string{"selector", "sort-by", "no-headers", "show-capacity"}},
	"events":         {PreviewRead, flags(printFlags, []string{"all-namespaces", "for", "types", "no-headers", "chunk-size"})},
	"explain":        {PreviewRead, []string{"recursive", "api-version", "output"}},
	"api-resources":  {PreviewRead, []string{"namespaced", "api-group", "verbs", "output", "sort-by", "no-headers", "cached", "categories"}},
	"api-versions":   {PreviewRead, nil},
	"version":        {PreviewRead, []string{"output", "client"}},
	"cluster-info":   {PreviewRead, nil},
	"wait":           {PreviewRead, flags(printFlags, []string{"all-namespaces", "selector", "field-selector", "for", "timeout", "all"})},
	"auth can-i":     {PreviewRead, []string{"all-namespaces", "list", "no-headers", "subresource", "quiet"}},
	"auth whoami":    {PreviewRead, []string{"output"}},
	"rollout status": {PreviewRead, []string{"selector", "revision", "timeout", "watch"}},
	"rollout history": {PreviewRead, flags(printFlags, []string{"selector", "revision"})},

	"apply":                        {PreviewChange, flags(changeFlags, []string{"filename", "server-side", "force-conflicts", "overwrite", "selector"})},
	"create":                       {PreviewChange, flags(changeFlags, []string{"filename", "save-config", "selector"})},
	"create deployment":            {PreviewChange, flags(changeFlags, []string{"image", "replicas", "port"})},
	"create job":                   {PreviewChange, flags(changeFlags, []string{"image", "from"})},
	"create cronjob":               {PreviewChange, flags(changeFlags, []string{"image", "schedule", "restart"})},
	"create configmap":             {PreviewChange, flags(changeFlags, []string{"from-literal", "append-hash"})},
	"create namespace":             {PreviewChange, changeFlags},
	"create serviceaccount":        {PreviewChange, changeFlags},
	"create role":                  {PreviewChange, flags(changeFlags, []string{"verb", "resource", "resource-name"})},
	"create clusterrole":           {PreviewChange, flags(changeFlags, []string{"verb", "resource", "resource-name", "non-resource-url", "aggregation-rule"})},
	"create rolebinding":           {PreviewChange, flags(changeFlags, []string{"role", "clusterrole", "user", "group", "serviceaccount"})},
	"create clusterrolebinding":    {PreviewChange, flags(changeFlags, []string{"clusterrole", "user", "group", "serviceaccount"})},
	"create quota":                 {PreviewChange, flags(changeFlags, []string{"hard", "scopes"})},
	"create priorityclass":         {PreviewChange, flags(changeFlags, []string{"value", "global-default", "description", "preemption-policy"})},
	"create poddisruptionbudget":   {PreviewChange, flags(changeFlags, []string{"selector", "min-available", "max-unavailable"})},
	"create ingress":               {PreviewChange, flags(changeFlags, []string{"class", "rule", "default-backend", "annotation"})},
	"create service clusterip":     {PreviewChange, flags(changeFlags, []string{"tcp", "clusterip"})},
	"create service nodeport":      {PreviewChange, flags(changeFlags, []string{"tcp", "node-port"})},
	"create service loadbalancer":  {PreviewChange, flags(changeFlags, []string{"tcp"})},
	"create service externalname":  {PreviewChange, flags(changeFlags, []string{"tcp", "external-name"})},
	"replace":                      {PreviewChange, flags(changeFlags, []string{"filename", "force", "grace-period", "cascade", "timeout", "save-config", "subresource"})},
	"patch":                        {PreviewChange, flags(changeFlags, []string{"filename", "patch", "type", "subresource"})},
	"label":                        {PreviewChange, flags(changeFlags, []string{"filename", "selector", "field-selector", "overwrite", "resource-version", "list"})},
	"annotate":                     {PreviewChange, flags(changeFlags, []string{"filename", "selector", "field-selector", "overwrite", "resource-version", "list"})},
	"scale":                        {PreviewChange, flags(printFlags, []string{"selector", "replicas", "current-replicas", "resource-version", "timeout"})},
	"set image":                    {PreviewChange, flags(changeFlags, []string{"selector"})},
	"set env":                      {PreviewChange, flags(changeFlags, []string{"selector", "env", "from", "prefix", "keys", "overwrite", "containers"})},
	"set resources":                {PreviewChange, flags(changeFlags, []string{"selector", "limits", "requests", "containers"})},
	"set selector":                 {PreviewChange, flags(changeFlags, []string{"resource-version"})},
	"set serviceaccount":           {PreviewChange, changeFlags},
	"set subject":                  {PreviewChange, flags(changeFlags, []string{"selector", "user", "group", "serviceaccount"})},
	"autoscale":                    {PreviewChange, flags(changeFlags, []string{"min", "max", "cpu-percent", "name"})},
	"expose":                       {PreviewChange, flags(changeFlags, []string{"port", "target-port", "protocol", "name", "type", "selector", "external-ip", "cluster-ip", "load-balancer-ip", "session-affinity", "labels", "overrides", "override-type"})},
	"run":                          {PreviewChange, flags(changeFlags, []string{"image", "port", "env", "labels", "annotations", "restart", "command", "overrides", "override-type", "image-pull-policy", "privileged", "expose"})},
	"taint":                        {PreviewChange, flags(changeFlags, []string{"selector", "overwrite"})},
	"rollout restart":              {PreviewChange, flags(changeFlags, []string{"selector"})},
	"rollout undo":                 {PreviewChange, flags(printFlags, []string{"selector", "to-revision"})},
	"rollout pause":                {PreviewChange, flags(changeFlags, []string{"selector"})},
	"rollout resume":               {PreviewChange, flags(changeFlags, []string{"selector"})},
	"auth reconcile":               {PreviewChange, flags(printFlags, []string{"filename", "remove-extra-permissions", "remove-extra-subjects"})},

	"delete":   {PreviewDryRun, []string{"filename", "selector", "field-selector", "grace-period", "force", "cascade", "wait", "timeout", "ignore-not-found", "now", "output"}},
	"drain":    {PreviewDryRun, []string{"selector", "pod-selector", "ignore-daemonsets", "delete-emptydir-data", "force", "grace-period", "timeout", "disable-eviction", "skip-wait-for-delete-timeout", "chunk-size"}},
	"cordon":   {PreviewDryRun, []string{"selector"}},
	"uncordon": {PreviewDryRun, []string{"selector"}},

	"exec": {PreviewNone, []string{"container", "stdin", "quiet", "pod-running-timeout"}},
}

// kubectlCommand is argv as kubectl reads it.
type kubectlCommand struct {
	// path is the command without "kubectl" ("rollout restart"); verb its first word.
	path, verb string
	preview    string
	// words are the positional arguments before "--"; command the container's command after it.
	words, command []string
	// flags holds each flag given, by kubectl's own (normalized) name, with its values.
	flags map[string][]string
	// stdin says whether the command reads stdin: `-f -` or `exec -i`.
	stdin bool
}

func (c kubectlCommand) has(flag string) bool { _, ok := c.flags[flag]; return ok }

// on says whether a switch is given and not set to false.
func (c kubectlCommand) on(flag string) bool { return c.has(flag) && c.value(flag) != "false" }

func (c kubectlCommand) value(flag string) string {
	if v := c.flags[flag]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

func invalidf(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, a...)...)
}

// unprintable says whether r changes how a word reads on the card: a control character or a
// bidirectional override.
func unprintable(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == '\u200b' || r == '\ufeff'
}

// parseKubectl reads argv with kubectl's own command tree and flag parser, at the version the image
// pins, so it reads every word as the binary will: spellings kubectl normalizes, values a flag
// takes as the next word, short-flag clusters. Then only the allowed commands and their allowed
// flags pass.
func parseKubectl(argv []string) (kubectlCommand, error) {
	if len(argv) < 2 || argv[0] != "kubectl" {
		return kubectlCommand{}, invalidf("argv is the command's words, starting with kubectl and then its verb (kubectl get pods -n widgets)")
	}
	for i, w := range argv {
		if strings.ContainsFunc(w, unprintable) {
			return kubectlCommand{}, invalidf("argv[%d] holds a control or bidirectional character", i)
		}
	}
	root := kubectlcmd.NewKubectlCommand(kubectlcmd.KubectlOptions{IOStreams: genericiooptions.NewTestIOStreamsDiscard()})
	sub, rest, err := root.Find(argv[1:])
	if err != nil || sub == root {
		return kubectlCommand{}, invalidf("argv names no kubectl command; the allowed ones: %s", strings.Join(sortedKeys(kubectlCommands), ", "))
	}
	path := strings.TrimPrefix(sub.CommandPath(), "kubectl ")
	c := kubectlCommand{path: path, verb: strings.Fields(path)[0], flags: map[string][]string{}}
	spec, ok := kubectlCommands[path]
	switch {
	case !ok && (path == "create token" || strings.HasPrefix(path, "create secret")):
		return c, invalidf("kubectl %s is refused: a secret comes from 1Password, through onepassword.item_to_secret", path)
	case !ok:
		return c, invalidf("kubectl %s is not allowed (allowed: %s)", path, strings.Join(sortedKeys(kubectlCommands), ", "))
	}
	c.preview = spec.preview
	if err := sub.ParseFlags(rest); err != nil {
		return c, invalidf("argv does not parse as kubectl %s: %s", path, err.Error())
	}
	fs := sub.Flags()
	allowed := slices.Concat(globalFlags, spec.flags)
	var bad error
	fs.Visit(func(f *pflag.Flag) {
		if bad != nil {
			return
		}
		if why, ok := refusedWhy[f.Name]; ok && !slices.Contains(allowed, f.Name) {
			bad = invalidf("--%s is refused for kubectl %s: %s", f.Name, path, why)
			return
		}
		if !slices.Contains(allowed, f.Name) {
			bad = invalidf("--%s is not allowed for kubectl %s (allowed: %s)", f.Name, path, strings.Join(allowed, ", "))
			return
		}
		vals := []string{f.Value.String()}
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			vals = sv.GetSlice()
		}
		for _, v := range vals {
			if strings.HasPrefix(v, "-") && v != "-" {
				bad = invalidf("--%s=%s: a value may not start with -", f.Name, v)
				return
			}
		}
		c.flags[f.Name] = vals
	})
	if bad != nil {
		return c, bad
	}
	args := fs.Args()
	c.words = args
	if at := fs.ArgsLenAtDash(); at >= 0 {
		c.words, c.command = args[:at], args[at:]
	}
	for _, f := range c.flags["filename"] {
		if f != "-" {
			return c, invalidf("-f takes only - (stdin): the command runs on the executor, whose files are not yours to read")
		}
		c.stdin = true
	}
	for _, o := range c.flags["output"] {
		if strings.Contains(o, "file") {
			return c, invalidf("-o %s reads a local file", o)
		}
	}
	switch {
	case c.verb == "exec":
		c.stdin = c.on("stdin")
		if len(c.command) == 0 {
			return c, invalidf("kubectl exec takes its command after --")
		}
	case len(c.command) > 0 && c.verb != "run" && c.verb != "create":
		return c, invalidf("kubectl %s takes nothing after --", path)
	}
	if err := c.admissionWriter(); err != nil {
		return c, err
	}
	return c, c.secretReader()
}

// admissionKind says whether a resource word names an admission object: a policy, a binding or a
// webhook configuration. The guard policy cannot protect these (the API server exempts them from
// policies), so no command may change them; the executor's proxy refuses the writes too.
func admissionKind(word string) bool {
	for _, part := range strings.Split(strings.ToLower(word), ",") {
		kind, _, _ := strings.Cut(part, "/")
		if strings.Contains(kind, "admissionregistration") || strings.Contains(kind, "admissionpolic") ||
			strings.Contains(kind, "webhookconfiguration") {
			return true
		}
	}
	return false
}

func (c kubectlCommand) admissionWriter() error {
	if c.preview == PreviewRead {
		return nil
	}
	for _, w := range c.words {
		if admissionKind(w) {
			return invalidf("kubectl %s of admission policies and webhooks is refused: change them through GitOps", c.path)
		}
	}
	return nil
}

// secretReader refuses commands that print Secret values: get, describe or events of Secrets, and
// wait on one, which confirms a guessed value. The executor's proxy refuses every Secret request
// too; this says why before anything runs.
func (c kubectlCommand) secretReader() error {
	if c.verb != "get" && c.verb != "describe" && c.verb != "events" && c.verb != "wait" {
		return nil
	}
	for _, w := range append(slices.Clone(c.words), c.flags["for"]...) {
		for _, part := range strings.Split(w, ",") {
			kind, _, _ := strings.Cut(strings.ToLower(part), "/")
			kind, _, _ = strings.Cut(kind, ".")
			if kind == "secret" || kind == "secrets" {
				return invalidf("kubectl %s of Secrets is refused: it prints their values", c.verb)
			}
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
