package privileged

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Bounds of a run's secrets and hosts.
const (
	MaxExecSecrets = 8
	MaxExecHosts   = 8
	// ExecSecretsVault is the one vault a run's secrets come from.
	ExecSecretsVault = "kubernetes"
	// reachPinned is the reach label of a run with secrets: not "internet", which the deployment's
	// policy opens to every public address, so only the run's own policy applies.
	reachPinned = "internet-pinned"
)

// ExecSecret asks for one 1Password field as a file in the run's pod, at /run/secrets/<Name>.
type ExecSecret struct {
	// Ref is op://kubernetes/<item>/<field>, an id or an exact title for each part.
	Ref  string `json:"ref"`
	Name string `json:"name"`
}

// ExecSecretFact is a frozen secret: its metadata, never its value.
type ExecSecretFact struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
	Item OPItem `json:"item"`
}

// DefaultGuardedItems are the item titles (globs) no run may take a secret from, whatever the
// owner approves: the executor's own credentials, 1Password's Connect operator's and op-gate's. A
// script with internet reach must never hold the keys that guard everything else. It is a name
// rule: an item named otherwise is not covered, so the list lives in the executor's configuration
// and is reviewed with it.
var DefaultGuardedItems = []string{
	"mainloop-executor-*", "op-gate*", "op-connect*", "op-credentials*", "onepassword*", "1password*",
}

var (
	execSecretNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,30}$`)
	// A host is a fully qualified lower-case name with at least two labels.
	execHostRE = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\.)+[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
)

// parseSecretRef splits op://kubernetes/<item>/<field>.
func parseSecretRef(ref string) (OPRef, error) {
	rest, ok := strings.CutPrefix(ref, "op://")
	parts := strings.Split(rest, "/")
	if !ok || len(parts) != 3 || slices.Contains(parts, "") {
		return OPRef{}, fmt.Errorf("ref is op://%s/<item>/<field>", ExecSecretsVault)
	}
	if parts[0] != ExecSecretsVault {
		return OPRef{}, fmt.Errorf("a run's secrets come from vault %q only", ExecSecretsVault)
	}
	for _, p := range parts {
		if !plainLine(p, maxOPRefLen) {
			return OPRef{}, errors.New("ref parts are one line each")
		}
	}
	return OPRef{Vault: parts[0], Item: parts[1], Field: parts[2]}, nil
}

// validateExecSecrets checks the shape of secrets and hosts.
func validateExecSecrets(a ExecArgs) error {
	switch {
	case len(a.Secrets) == 0 && len(a.Hosts) == 0:
		return nil
	case len(a.Secrets) == 0:
		return invalidf("hosts are for a run with secrets: a run without them reaches the internet, and the owner sees its script")
	case len(a.Hosts) == 0:
		return invalidf("a run with secrets names the hosts it may send them to (hosts)")
	case a.Reach != ReachInternet:
		return invalidf("secrets need reach %q: a run with the cluster's credentials has no internet, and a secret goes to named hosts only", ReachInternet)
	case len(a.Secrets) > MaxExecSecrets:
		return invalidf("at most %d secrets", MaxExecSecrets)
	case len(a.Hosts) > MaxExecHosts:
		return invalidf("at most %d hosts", MaxExecHosts)
	}
	names, refs := map[string]bool{}, map[string]bool{}
	for _, s := range a.Secrets {
		switch {
		case !execSecretNameRE.MatchString(s.Name):
			return invalidf("a secret's name is 1 to 31 lower-case letters, digits, '-' or '_', starting with a letter (it is the file /run/secrets/<name>)")
		case names[s.Name]:
			return invalidf("the secret name %q is used twice", s.Name)
		case refs[s.Ref]:
			return invalidf("the secret %q is asked for twice", s.Ref)
		}
		if _, err := parseSecretRef(s.Ref); err != nil {
			return invalidf("%s", err.Error())
		}
		names[s.Name], refs[s.Ref] = true, true
	}
	seen := map[string]bool{}
	for _, h := range a.Hosts {
		switch {
		case len(h) > 253 || !execHostRE.MatchString(h):
			return invalidf("host %q is a fully qualified lower-case name (no wildcard, port or address)", h)
		case isAddress(h) || internalName(h):
			return invalidf("host %q is not a public name", h)
		case seen[h]:
			return invalidf("host %q is listed twice", h)
		}
		seen[h] = true
	}
	return nil
}

func isAddress(h string) bool {
	_, err := netip.ParseAddr(h)
	return err == nil
}

// internalName says whether a name is one the cluster or a home network resolves.
func internalName(h string) bool {
	for _, suffix := range []string{".local", ".internal", ".lan", ".home", ".home.arpa", ".svc", ".cluster", ".localhost", ".olds.network"} {
		if h == strings.TrimPrefix(suffix, ".") || strings.HasSuffix(h, suffix) {
			return true
		}
	}
	return false
}

// guarded reports whether an item may not be a run's secret.
func guarded(patterns []string, o OPItem) bool {
	if len(patterns) == 0 {
		patterns = DefaultGuardedItems
	}
	for _, name := range []string{o.ItemTitle, o.ItemID} {
		for _, p := range patterns {
			if ok, _ := path.Match(strings.ToLower(p), strings.ToLower(name)); ok {
				return true
			}
		}
	}
	return false
}

// preflightExecSecrets reads each secret's metadata (never its value) and refuses a guarded item,
// another vault, an empty field and one 1Password does not expose.
func preflightExecSecrets(ctx context.Context, s *Service, a ExecArgs) ([]ExecSecretFact, error) {
	if len(a.Secrets) == 0 {
		return nil, nil
	}
	if !s.ToolboxSecrets || s.OnePassword == nil || s.Toolbox == nil {
		return nil, refuse("this executor's toolbox does not take secrets")
	}
	var out []ExecSecretFact
	for _, sec := range a.Secrets {
		ref, err := parseSecretRef(sec.Ref)
		if err != nil {
			return nil, refuse("%s", err.Error())
		}
		item, err := s.OnePassword.Lookup(ctx, ref)
		if errors.Is(err, ErrBadTarget) {
			return nil, refuse("%s", strings.TrimPrefix(err.Error(), ErrBadTarget.Error()+": "))
		}
		if err != nil {
			return nil, err
		}
		switch {
		case item.VaultTitle != ExecSecretsVault:
			return nil, refuse("secret %q is not in vault %q", sec.Name, ExecSecretsVault)
		case guarded(s.ToolboxGuardedItems, item):
			return nil, refuse("item %q is one the executor, the 1Password operator or op-gate depends on: a run may not take it", item.ItemTitle)
		case item.Empty:
			return nil, refuse("field %q of item %q has no value", item.FieldTitle, item.ItemTitle)
		case item.FieldType == "Unsupported":
			return nil, refuse("field %q of item %q has a type 1Password does not expose", item.FieldTitle, item.ItemTitle)
		}
		out = append(out, ExecSecretFact{Name: sec.Name, Ref: sec.Ref, Item: item})
	}
	return out, nil
}

// readExecSecrets reads each frozen secret's value for the run's pod. An item whose version
// changed since the owner saw it is stale, and so is one now guarded. The values are the caller's
// to clear.
func (r *Runner) readExecSecrets(ctx context.Context, facts []ExecSecretFact) (map[string][]byte, string, error) {
	values := map[string][]byte{}
	for _, f := range facts {
		if guarded(r.ToolboxGuardedItems, f.Item) {
			return values, "item " + f.Item.ItemTitle + " is now one a run may not take", nil
		}
		item, v, err := r.OnePassword.Value(ctx, f.Item)
		switch {
		case errors.Is(err, ErrBadTarget):
			return values, "the 1Password item or field " + f.Item.ItemTitle + " is gone", nil
		case err != nil:
			return values, "", scrubbed(err, v)
		case item.Version != f.Item.Version:
			clear(v)
			return values, fmt.Sprintf("the 1Password item %s changed since the owner saw it (version %d, now %d)", f.Item.ItemTitle, f.Item.Version, item.Version), nil
		case len(v) == 0:
			return values, "the field " + f.Item.FieldTitle + " of " + f.Item.ItemTitle + " has no value", nil
		}
		values[f.Name] = v
	}
	return values, "", nil
}

// redactions are the strings to remove from a run's output for these secret values: each whole
// value, and each longer line of a multi-line one (a certificate a script prints in pieces).
func redactions(values map[string][]byte) []string {
	var out []string
	for _, v := range values {
		s := string(v)
		out = append(out, s)
		if strings.Contains(strings.TrimSpace(s), "\n") {
			for line := range strings.SplitSeq(s, "\n") {
				if line = strings.TrimSpace(line); len(line) >= 8 {
					out = append(out, line)
				}
			}
		}
	}
	return out
}
