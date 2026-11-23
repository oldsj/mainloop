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

	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// TypeItemToSecret copies one field of a 1Password item into one key of a Kubernetes Secret.
const TypeItemToSecret = "onepassword.item_to_secret"

// ErrBadTarget is a remote target the action cannot apply to as asked: a vault, item, field or
// Secret that is missing, ambiguous or not allowed. Its message is shown to the agent.
var ErrBadTarget = errors.New("privileged: the target cannot be used as asked")

// ErrTargetChanged is a write the remote refused because its target changed since preflight.
var ErrTargetChanged = errors.New("privileged: the target changed")

// ErrForbidden is a write the executor's credential is not allowed to make.
var ErrForbidden = errors.New("privileged: the executor may not do that")

// OnePassword reads items through the executor's read-only 1Password service account.
type OnePassword interface {
	// Lookup finds the vault, item and field by id or exact title and returns their metadata only.
	// A missing or ambiguous one is ErrBadTarget.
	Lookup(ctx context.Context, ref OPRef) (OPItem, error)
	// Value reads the field Lookup found, by id, with the item's current metadata. A field or item
	// that is gone is ErrBadTarget.
	Value(ctx context.Context, item OPItem) (OPItem, []byte, error)
}

// OPRef names a field: each part is an id or an exact title.
type OPRef struct {
	Vault string
	Item  string
	Field string
}

// OPItem is a field's metadata. It never carries the value.
type OPItem struct {
	VaultID    string    `json:"vault_id"`
	VaultTitle string    `json:"vault_title"`
	ItemID     string    `json:"item_id"`
	ItemTitle  string    `json:"item_title"`
	Category   string    `json:"category"`
	Version    uint32    `json:"version"`
	UpdatedAt  time.Time `json:"updated_at"`
	FieldID    string    `json:"field_id"`
	FieldTitle string    `json:"field_title"`
	FieldType  string    `json:"field_type"`
	Section    string    `json:"section,omitempty"`
	// Empty says the field has no value; preflight refuses it.
	Empty bool `json:"-"`
}

// ItemToSecretArgs are onepassword.item_to_secret's arguments.
type ItemToSecretArgs struct {
	Vault     string `json:"vault"`
	Item      string `json:"item"`
	Field     string `json:"field"`
	Namespace string `json:"namespace"`
	Secret    string `json:"secret"`
	Key       string `json:"key"`
}

// ItemToSecretFacts are onepassword.item_to_secret's frozen facts: metadata only, never a value.
type ItemToSecretFacts struct {
	Item      OPItem     `json:"item"`
	Target    SecretMeta `json:"target"`
	CheckedAt time.Time  `json:"checked_at"`
}

// ItemToSecretResult is onepassword.item_to_secret's result. It never carries the value or a hash
// of it.
type ItemToSecretResult struct {
	Namespace       string `json:"namespace"`
	Secret          string `json:"secret"`
	Key             string `json:"key"`
	ResourceVersion string `json:"resource_version,omitempty"`
	Created         bool   `json:"created,omitempty"`
	// Observed is set when an earlier attempt had already written it.
	Observed bool `json:"observed,omitempty"`
}

const maxOPRefLen = 200

var (
	dnsLabelRE     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	dnsSubdomainRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
	secretKeyRE    = regexp.MustCompile(`^[-._a-zA-Z0-9]{1,253}$`)
)

// deniedNamespace is a namespace no Secret is ever written to, whatever the executor's allowlist
// says: the cluster's own, the 1Password operator's, Argo CD's and Mainloop's.
func deniedNamespace(ns string) bool {
	for _, p := range []string{"kube-", "mainloop", "onepassword", "argocd"} {
		if strings.HasPrefix(ns, p) {
			return true
		}
	}
	return ns == "default"
}

func normalizeItemToSecret(raw json.RawMessage) (any, error) {
	var a ItemToSecretArgs
	if err := decodeStrict(raw, &a); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		name string
		v    *string
	}{{"vault", &a.Vault}, {"item", &a.Item}, {"field", &a.Field}} {
		*f.v = strings.TrimSpace(*f.v)
		if !plainLine(*f.v, maxOPRefLen) {
			return nil, fmt.Errorf("%w: %s must be one line of 1 to %d characters (an id or the exact title)", ErrInvalid, f.name, maxOPRefLen)
		}
	}
	switch {
	case !dnsLabelRE.MatchString(a.Namespace):
		return nil, fmt.Errorf("%w: namespace must be a Kubernetes namespace name", ErrInvalid)
	case deniedNamespace(a.Namespace):
		return nil, fmt.Errorf("%w: Secrets are never written to namespace %q", ErrInvalid, a.Namespace)
	case !dnsSubdomainRE.MatchString(a.Secret):
		return nil, fmt.Errorf("%w: secret must be a Kubernetes object name", ErrInvalid)
	case !secretKeyRE.MatchString(a.Key) || a.Key == "." || a.Key == "..":
		return nil, fmt.Errorf("%w: key must be 1 to 253 letters, digits, '-', '_' or '.'", ErrInvalid)
	}
	return a, nil
}

func preflightItemToSecret(ctx context.Context, s *Service, raw json.RawMessage, now time.Time) (any, error) {
	var a ItemToSecretArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if s.OnePassword == nil || s.Secrets == nil {
		return nil, refuse("this executor has no 1Password service account or no Kubernetes access")
	}
	if deniedNamespace(a.Namespace) || !slices.Contains(s.SecretNamespaces, a.Namespace) {
		return nil, refuse("the executor may not write Secrets in namespace %q (allowed: %s)", a.Namespace, orNone(s.SecretNamespaces))
	}
	item, err := s.OnePassword.Lookup(ctx, OPRef{Vault: a.Vault, Item: a.Item, Field: a.Field})
	if errors.Is(err, ErrBadTarget) {
		return nil, refuse("%s", strings.TrimPrefix(err.Error(), ErrBadTarget.Error()+": "))
	}
	if err != nil {
		return nil, err
	}
	switch {
	case item.Empty:
		return nil, refuse("field %q of item %q has no value", item.FieldTitle, item.ItemTitle)
	case item.FieldType == "Unsupported":
		return nil, refuse("field %q of item %q has a type 1Password does not expose", item.FieldTitle, item.ItemTitle)
	}
	target, err := s.Secrets.Inspect(ctx, a.Namespace, a.Secret)
	if err != nil {
		return nil, err
	}
	if target.Exists {
		switch {
		case target.Controller != "":
			return nil, refuse("Secret %s/%s is owned by %s, which would overwrite the change", a.Namespace, a.Secret, target.Controller)
		case target.ArgoTracked:
			return nil, refuse("Secret %s/%s is managed by Argo CD; change it in Git", a.Namespace, a.Secret)
		case target.Type != "Opaque":
			return nil, refuse("Secret %s/%s has type %s; only Opaque Secrets are written", a.Namespace, a.Secret, target.Type)
		}
	}
	return ItemToSecretFacts{Item: item, Target: target, CheckedAt: now}, nil
}

func renderItemToSecret(r dbq.PrivilegedAction, ctx map[string]any) (string, string, map[string]any, error) {
	var f ItemToSecretFacts
	if err := json.Unmarshal(r.FrozenFacts, &f); err != nil {
		return "", "", nil, err
	}
	var a ItemToSecretArgs
	if err := json.Unmarshal(r.Args, &a); err != nil {
		return "", "", nil, err
	}
	it := f.Item
	title := fmt.Sprintf("Copy 1Password %q to Secret %s/%s", it.ItemTitle+" › "+it.FieldTitle, a.Namespace, a.Secret)
	var b strings.Builder
	fmt.Fprintf(&b, "From 1Password: vault %q, item %q (%s, version %d, updated %s), field %q (%s",
		it.VaultTitle, it.ItemTitle, it.Category, it.Version, it.UpdatedAt.UTC().Format(time.RFC3339), it.FieldTitle, it.FieldType)
	if it.Section != "" {
		fmt.Fprintf(&b, ", section %q", it.Section)
	}
	b.WriteString(").\n")
	fmt.Fprintf(&b, "To: Secret %s/%s, key %q.\n", a.Namespace, a.Secret, a.Key)
	var tags []string
	t := f.Target
	if !t.Exists {
		b.WriteString("The Secret does not exist; Mainloop creates it (type Opaque) with this one key.\n")
	} else {
		fmt.Fprintf(&b, "The Secret exists with keys: %s. ", orNone(t.Keys))
		if slices.Contains(t.Keys, a.Key) {
			fmt.Fprintf(&b, "Key %q is replaced; the others are left as they are.\n", a.Key)
			tags = append(tags, "overwrites an existing key")
		} else {
			fmt.Fprintf(&b, "Key %q is added; the others are left as they are.\n", a.Key)
		}
		if t.ManagedBy != "" {
			tags = append(tags, "Secret labelled managed-by "+t.ManagedBy)
		}
	}
	b.WriteString("If the item or the Secret changes before this runs, nothing is written.")
	if len(tags) > 0 {
		fmt.Fprintf(&b, "\nRisk: %s.", strings.Join(tags, ", "))
	}
	ctx["namespace"] = a.Namespace
	return title, b.String(), ctx, nil
}

func (r *Runner) itemToSecret(ctx context.Context, row dbq.PrivilegedAction) outcome {
	var f ItemToSecretFacts
	var a ItemToSecretArgs
	if json.Unmarshal(row.FrozenFacts, &f) != nil || json.Unmarshal(row.Args, &a) != nil {
		return outcome{state: StateFailed, err: "the frozen facts cannot be read"}
	}
	if r.OnePassword == nil || r.Secrets == nil {
		return outcome{state: StateFailed, err: "this executor has no 1Password service account or no Kubernetes access"}
	}
	res := ItemToSecretResult{Namespace: a.Namespace, Secret: a.Secret, Key: a.Key}
	// Observe first: an earlier attempt may have written it.
	now, err := r.Secrets.Inspect(ctx, a.Namespace, a.Secret)
	if err != nil {
		return retry(err)
	}
	switch {
	case now.Exists && now.Action == row.ID:
		res.ResourceVersion, res.Observed = now.ResourceVersion, true
		return outcome{state: StateSucceeded, result: res}
	case f.Target.Exists && !now.Exists:
		return outcome{state: StateStale, err: "the Secret was deleted since the owner saw it"}
	case f.Target.Exists && (now.UID != f.Target.UID || now.ResourceVersion != f.Target.ResourceVersion):
		return outcome{state: StateStale, err: "the Secret changed since the owner saw it"}
	case !f.Target.Exists && now.Exists:
		return outcome{state: StateStale, err: "the Secret was created by someone else since the owner saw it"}
	}
	item, value, err := r.OnePassword.Value(ctx, f.Item)
	defer clear(value)
	switch {
	case errors.Is(err, ErrBadTarget):
		return outcome{state: StateStale, err: "the 1Password item or field is gone"}
	case err != nil:
		return retry(scrubbed(err, value))
	case item.Version != f.Item.Version:
		return outcome{state: StateStale, err: fmt.Sprintf("the 1Password item changed since the owner saw it (version %d, now %d)", f.Item.Version, item.Version)}
	case len(value) == 0:
		return outcome{state: StateFailed, err: "the field has no value"}
	}
	var rv string
	if f.Target.Exists {
		rv, err = r.Secrets.Patch(ctx, a.Namespace, a.Secret, a.Key, value, row.ID, f.Target.ResourceVersion)
	} else {
		rv, err = r.Secrets.Create(ctx, a.Namespace, a.Secret, a.Key, value, row.ID)
		res.Created = err == nil
	}
	switch {
	case errors.Is(err, ErrTargetChanged):
		return outcome{state: StateStale, err: "the Secret changed while it was written; nothing was written"}
	case errors.Is(err, ErrForbidden):
		return outcome{state: StateFailed, err: safeError(scrubbed(err, value))}
	case err != nil:
		return retry(scrubbed(err, value)) // unknown whether it was written; the next attempt observes
	}
	res.ResourceVersion = rv
	return outcome{state: StateSucceeded, result: res}
}

// scrubbed removes a secret value from an error's text, should a remote ever echo it back.
func scrubbed(err error, value []byte) error {
	if err == nil || len(value) < 4 || !strings.Contains(err.Error(), string(value)) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), string(value), "[value removed]"))
}

func orNone(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, ", ")
}
