package privileged

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The omnictl and talosctl the prod image pins. Their argv is read by the allowlist below, written
// against these releases' command trees.
const (
	OmnictlVersion  = "v1.13.1"
	TalosctlVersion = "v1.14.5"
)

// The two Sidero tools.
const (
	ToolOmnictl  = "omnictl"
	ToolTalosctl = "talosctl"
)

// sideroFlag is how one allowed flag is written: a switch (--x, --x=true, --x=false) or a value
// (--x=v only). values, if set, are the only values it may take.
type sideroFlag struct {
	value  bool
	values []string
	// require, if set, is the one value the flag must be given, and it must be given.
	require string
	// check, if set, says whether a value is allowed; hint says what is.
	check func(string) bool
	hint  string
}

var (
	switchFlag = sideroFlag{}
	valueFlag  = sideroFlag{value: true}
	outputFlag = sideroFlag{value: true, values: []string{"table", "yaml", "json"}}
	// noWait: talosctl tracks a reboot, shutdown, reset or upgrade until the node is back, which
	// can take longer than any command's deadline. The command returns once the node accepts it;
	// later reads show the rest.
	noWait = sideroFlag{require: "false"}
)

// sideroSpec is one allowed omnictl or talosctl command.
type sideroSpec struct {
	preview string
	// args are the fewest and most positional words.
	args  [2]int
	flags map[string]sideroFlag
	// destructive says how the command deletes or wipes, for the card's tags and a delete's
	// expiry; "" when it does not.
	destructive string
	// effect is the card's tag for a change that is not destructive.
	effect string
	// dryRun: preflight runs the command with --dry-run, which only reads, and shows its text.
	dryRun bool
	// stdin: --file=- reads the request's stdin.
	stdin bool
}

// The commands each tool may run: an allowlist, so a command or flag a release adds later is
// refused until it is read and added here. Every talosctl command also takes --nodes, which it
// requires. Neither tool may be given a flag that chooses the endpoint, the cluster, the
// configuration or the credential: the executor sets those.
var sideroCommands = map[string]map[string]sideroSpec{
	ToolOmnictl: {
		"get": {preview: PreviewRead, args: [2]int{1, 2}, flags: map[string]sideroFlag{
			"namespace": valueFlag, "output": outputFlag, "selector": valueFlag, "id-match-regexp": valueFlag,
		}},
		"cluster status": {preview: PreviewRead, args: [2]int{1, 1}, flags: map[string]sideroFlag{"quiet": switchFlag}},
		"apply": {preview: PreviewDryRun, dryRun: true, stdin: true, effect: "creates or updates Omni resources", flags: map[string]sideroFlag{
			"file": {value: true, require: "-"}, "verbose": switchFlag,
		}},
		// delete names one resource: the executor's proxy reads it to check its cluster, which a
		// selector's sweep would not allow before it ran.
		"delete": {preview: PreviewNone, args: [2]int{2, 2}, destructive: "deletes an Omni resource", flags: map[string]sideroFlag{
			"namespace": valueFlag,
		}},
		"cluster delete": {preview: PreviewDryRun, dryRun: true, args: [2]int{1, 1}, destructive: "deletes the cluster", flags: map[string]sideroFlag{
			"verbose": switchFlag, "destroy-disconnected-machines": switchFlag,
		}},
	},
	ToolTalosctl: {
		"version":                 {preview: PreviewRead, flags: map[string]sideroFlag{"short": switchFlag, "json": switchFlag}},
		"get":                     {preview: PreviewRead, args: [2]int{1, 2}, flags: map[string]sideroFlag{"namespace": valueFlag, "output": outputFlag}},
		"service":                 {preview: PreviewRead, args: [2]int{0, 2}},
		"logs":                    {preview: PreviewRead, args: [2]int{1, 1}, flags: map[string]sideroFlag{"tail": valueFlag, "kubernetes": switchFlag, "namespace": valueFlag}},
		"etcd members":            {preview: PreviewRead},
		"etcd status":             {preview: PreviewRead},
		"etcd alarm list":         {preview: PreviewRead},
		"containers":              {preview: PreviewRead, flags: map[string]sideroFlag{"kubernetes": switchFlag, "namespace": valueFlag}},
		"stats":                   {preview: PreviewRead, flags: map[string]sideroFlag{"kubernetes": switchFlag, "namespace": valueFlag}},
		"memory":                  {preview: PreviewRead, flags: map[string]sideroFlag{"verbose": switchFlag}},
		"processes":               {preview: PreviewRead, flags: map[string]sideroFlag{"sort": {value: true, values: []string{"rss", "cpu"}}}},
		"mounts":                  {preview: PreviewRead},
		"netstat":                 {preview: PreviewRead, flags: netstatFlags()},
		"time":                    {preview: PreviewRead, flags: map[string]sideroFlag{"check": valueFlag}},
		"list":                    {preview: PreviewRead, args: [2]int{0, 1}, flags: map[string]sideroFlag{"long": switchFlag, "recurse": switchFlag, "humanize": switchFlag, "type": valueFlag}},
		"reboot":                  {preview: PreviewNone, effect: "reboots the node", flags: map[string]sideroFlag{"wait": noWait, "mode": {value: true, values: []string{"default", "powercycle"}}}},
		"shutdown":                {preview: PreviewNone, destructive: "powers the node off; it stays off until someone powers it on", flags: map[string]sideroFlag{"wait": noWait, "force": switchFlag}},
		"reset":                   {preview: PreviewNone, destructive: "wipes the node", flags: resetFlags()},
		"upgrade":                 {preview: PreviewNone, effect: "upgrades Talos and reboots the node", flags: upgradeFlags()},
		"rollback":                {preview: PreviewNone, effect: "rolls Talos back and reboots the node"},
		"etcd defrag":             {preview: PreviewNone, effect: "defragments etcd's database (it blocks etcd while it runs)"},
		"etcd forfeit-leadership": {preview: PreviewNone, effect: "moves etcd's leadership"},
		"etcd alarm disarm":       {preview: PreviewNone, effect: "disarms etcd's alarms"},
		"etcd leave":              {preview: PreviewNone, destructive: "removes the node from etcd"},
		"etcd remove-member":      {preview: PreviewNone, args: [2]int{1, 1}, destructive: "removes a member from etcd"},
	},
}

func netstatFlags() map[string]sideroFlag {
	out := map[string]sideroFlag{}
	for _, f := range []string{"verbose", "extend", "programs", "timers", "listening", "all", "pods", "tcp", "udp", "udplite", "raw", "ipv4", "ipv6"} {
		out[f] = switchFlag
	}
	return out
}

func resetFlags() map[string]sideroFlag {
	return map[string]sideroFlag{
		"wait": noWait, "graceful": switchFlag, "reboot": switchFlag,
		"wipe-mode":             {value: true, values: []string{"all", "system-disk", "user-disks"}},
		"user-disks-to-wipe":    valueFlag,
		"system-labels-to-wipe": valueFlag,
	}
}

func upgradeFlags() map[string]sideroFlag {
	return map[string]sideroFlag{
		"wait": noWait, "image": {value: true, check: talosImage, hint: installerImageHint},
		"stage": switchFlag, "force": switchFlag, "preserve": switchFlag,
		"reboot-mode": {value: true, values: []string{"default", "powercycle"}},
	}
}

// sideroRefusedWhy says why a flag no command allows is refused, for the flags someone is likely
// to try. The decision is the allowlist; this is only the message.
var sideroRefusedWhy = map[string]string{
	"omniconfig": "the executor sets the endpoint and credential", "talosconfig": "the executor sets the endpoint and credential",
	"context": "the executor sets the target", "cluster": "the executor sets the target (the request's target)",
	"endpoints": "the executor sets the endpoint", "insecure-skip-tls-verify": "the executor sets the endpoint",
	"insecure": "the executor sets the endpoint", "siderov1-keys-dir": "the executor sets the credential",
	"watch": "it never ends", "follow": "it never ends", "dry-run": "the executor runs the dry run itself",
	"all": "name the resources, or select them with --selector", "debug": "it never ends",
	"timeout": "the request's timeout_seconds bounds the run", "wait": "give --wait=false: the command returns once the node accepts it",
}

// sideroCommand is a parsed omnictl or talosctl command.
type sideroCommand struct {
	tool string
	spec sideroSpec
	path string
	// words are the positional words.
	words []string
	// flags are the flags given, by name, without the leading dashes.
	flags map[string]string
	nodes []string
	// resourceType is the canonical type get or delete names.
	resourceType string
}

func (c sideroCommand) on(name string) bool { v, ok := c.flags[name]; return ok && v != "false" }

// preview is how preflight previews the command. talosctl service reads unless it starts, stops
// or restarts.
func (c sideroCommand) preview() string {
	if c.tool == ToolTalosctl && c.path == "service" && len(c.words) == 2 && c.words[1] != "status" {
		return PreviewNone
	}
	return c.spec.preview
}

// effect is the card's tag for what the command changes, or "".
func (c sideroCommand) effect() string {
	if c.tool == ToolTalosctl && c.path == "service" && c.preview() == PreviewNone {
		return c.words[1] + "s a service"
	}
	if image, ok := c.flags["image"]; ok && c.path == "upgrade" {
		return "installs " + image + " as the node's operating system, which then has full control of the node, and reboots it"
	}
	return c.spec.effect
}

// Secret-bearing resource types: no command reads or writes them, and the executor's proxy refuses
// every request naming one, whatever the command. Omni's cluster and machine secrets, configs
// (which embed them), kubeconfigs, talosconfigs, join and node tokens, backups, encryption and BMC
// settings, identities and access policies; Talos's secrets namespace, machine configs and
// KubeSpan identity.
var secretTypeRE = regexp.MustCompile(`(?i)secret|token|key|identit|user|accesspolic|saml|auth|permission|` +
	`kubeconfig|talosconfig|joinconfig|machineconfig|configdiff|encryption|bmc|s3config|backupdata|` +
	`connectionparam|siderolinkapiconfig|rotat|serviceaccount|certificate`)

// redactedTypes are secret-shaped names that hold no secret: Omni redacts them.
var redactedTypes = []string{"redactedclustermachineconfigs.omni.sidero.dev", "redactedclustermachineconfig", "redactedclustermachineconfigs"}

// secretNamespaces are COSI namespaces that hold only secrets.
var secretNamespaces = []string{"secrets"}

// secretType says whether a resource type (or a type alias given on the command line) may hold a
// secret.
func secretType(t string) bool {
	if slices.Contains(redactedTypes, strings.ToLower(t)) {
		return false
	}
	return secretTypeRE.MatchString(t)
}

// parseSidero reads an omnictl or talosctl argv by a strict subset of its grammar, which cobra
// and pflag read the same way: the command's words first, then long flags only (--name or
// --name=value, never a separate value word, never a short flag or a cluster of them) and
// positional words that do not start with "-". There is no "--".
func parseSidero(tool string, argv []string) (sideroCommand, error) {
	cmds, ok := sideroCommands[tool]
	if !ok {
		return sideroCommand{}, invalidf("the tool is omnictl or talosctl")
	}
	if len(argv) < 2 || argv[0] != tool {
		return sideroCommand{}, invalidf("argv starts with %q and a command, e.g. [%q, \"get\", ...]", tool, tool)
	}
	for i, w := range argv {
		if strings.ContainsFunc(w, unprintable) {
			return sideroCommand{}, invalidf("argv[%d] holds a control or invisible character", i)
		}
	}
	c := sideroCommand{tool: tool, flags: map[string]string{}}
	rest := argv[1:]
	// The longest allowed command its leading words name.
	for n := min(3, len(rest)); n > 0; n-- {
		path := strings.Join(rest[:n], " ")
		if spec, ok := cmds[path]; ok {
			c.path, c.spec, rest = path, spec, rest[n:]
			break
		}
	}
	if c.path == "" {
		return c, invalidf("%s %s is not a command the executor runs (it runs: %s)", tool, argv[1], strings.Join(sortedKeys(cmds), ", "))
	}
	for _, w := range rest {
		switch {
		case w == "--":
			return c, invalidf("\"--\" is refused: give flags as --name=value and words after them")
		case strings.HasPrefix(w, "--"):
			name, value, hasValue := strings.Cut(w[2:], "=")
			if err := c.flag(name, value, hasValue); err != nil {
				return c, err
			}
		case strings.HasPrefix(w, "-"):
			return c, invalidf("%q: short flags are refused; give the long name as --name=value", w)
		case w == "":
			return c, invalidf("an empty word is refused")
		default:
			c.words = append(c.words, w)
		}
	}
	for name, f := range c.spec.flags {
		if f.require != "" {
			if v, ok := c.flags[name]; !ok || v != f.require {
				return c, invalidf("%s %s needs --%s=%s", tool, c.path, name, f.require)
			}
		}
	}
	if min, max := c.spec.args[0], c.spec.args[1]; len(c.words) < min || len(c.words) > max {
		return c, invalidf("%s %s takes %d to %d words besides its flags; it was given %d", tool, c.path, min, max, len(c.words))
	}
	if tool == ToolTalosctl {
		if len(c.nodes) == 0 {
			return c, invalidf("talosctl needs --nodes=<node>[,<node>...]: the executor never picks a node")
		}
		if _, ok := c.flags["image"]; c.path == "upgrade" && !ok {
			return c, invalidf("talosctl upgrade needs --image=<%s>: the executor checks it against the node's schematic and version", installerImageHint)
		}
		if c.path == "service" && len(c.words) == 2 && !slices.Contains([]string{"start", "stop", "restart", "status"}, c.words[1]) {
			return c, invalidf("talosctl service <id> takes start, stop, restart or status")
		}
	}
	if (c.path == "get" || c.path == "delete") && len(c.words) > 0 {
		if secretType(c.words[0]) {
			return c, invalidf("%s may hold secrets: the executor never reads or writes it", c.words[0])
		}
		t, ok := sideroTypeFor(tool, c.words[0])
		if !ok {
			return c, invalidf("%s is not a resource type the executor reads; give one of these by its full name: %s", c.words[0], typeNames(tool))
		}
		c.resourceType = t
	}
	if c.path == "get" && slices.Contains(secretNamespaces, c.flags["namespace"]) {
		return c, invalidf("the %s namespace holds secrets: the executor never reads it", c.flags["namespace"])
	}
	return c, nil
}

func (c *sideroCommand) flag(name, value string, hasValue bool) error {
	if name == "" || strings.ContainsAny(name, "_ ") || strings.ToLower(name) != name {
		return invalidf("--%s is not a flag name the executor reads", name)
	}
	if _, dup := c.flags[name]; dup || (name == "nodes" && c.nodes != nil) {
		return invalidf("--%s is given twice", name)
	}
	if c.tool == ToolTalosctl && name == "nodes" {
		if !hasValue || value == "" {
			return invalidf("--nodes takes a value: --nodes=<node>[,<node>...]")
		}
		for n := range strings.SplitSeq(value, ",") {
			if !nodeRE.MatchString(n) {
				return invalidf("--nodes: %q is not a node address or name", n)
			}
			c.nodes = append(c.nodes, n)
		}
		return nil
	}
	f, ok := c.spec.flags[name]
	if !ok {
		if why, ok := sideroRefusedWhy[name]; ok {
			return invalidf("--%s is refused: %s", name, why)
		}
		return invalidf("%s %s does not take --%s (it takes: %s)", c.tool, c.path, name, strings.Join(c.flagNames(), ", "))
	}
	switch {
	case f.value && !hasValue:
		return invalidf("--%s takes a value: --%s=<value>", name, name)
	case !f.value && f.require == "" && hasValue && value != "true" && value != "false":
		return invalidf("--%s is a switch: --%s, --%s=true or --%s=false", name, name, name, name)
	case !f.value && !hasValue:
		value = "true"
	case strings.HasPrefix(value, "-") && value != "-":
		return invalidf("--%s=%s: a value starting with \"-\" is refused", name, value)
	case len(value) > 256:
		return invalidf("--%s: the value is over 256 characters", name)
	case f.values != nil && !slices.Contains(f.values, value):
		return invalidf("--%s takes %s", name, strings.Join(f.values, ", "))
	case f.check != nil && !f.check(value):
		return invalidf("--%s takes %s", name, f.hint)
	}
	c.flags[name] = value
	return nil
}

func (c sideroCommand) flagNames() []string {
	out := sortedKeys(c.spec.flags)
	if c.tool == ToolTalosctl {
		out = append(out, "nodes")
	}
	if len(out) == 0 {
		return []string{"none"}
	}
	return out
}

// nodeRE is a node's address or hostname.
var nodeRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.:-]{0,252}[A-Za-z0-9])?$`)

// sideroArgv is the argv the binary runs: the approved one, with --file=- reading the file the
// executor writes stdin to, and with --dry-run for a preview.
func sideroArgv(argv []string, stdinFile string, dryRun bool) []string {
	out := make([]string, 0, len(argv))
	for _, w := range argv[1:] {
		if w == "--file=-" {
			w = "--file=" + stdinFile
		}
		out = append(out, w)
	}
	if dryRun {
		out = append(out, "--dry-run")
	}
	return out
}

func (c sideroCommand) String() string { return fmt.Sprintf("%s %s", c.tool, c.path) }
