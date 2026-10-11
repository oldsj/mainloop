package environments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// Static OCI validation reads image metadata only (manifests and the config blob), never layers.
const (
	// ValidatorVersion names the validation policy. A stored version with another value is stale.
	ValidatorVersion = "oci-static-v2"
	// MaxMetadataBytes caps every metadata object the registry client returns.
	MaxMetadataBytes = 4 << 20
	// RequiredUser is the only USER an environment image may declare.
	RequiredUser = "65532:65532"
)

// Version statuses, provenance kinds and sources, as stored.
const (
	StatusPendingBuild    = "pending_build"
	StatusStaticValidated = "static_validated"

	ProvenanceUserPushed    = "user_pushed"
	ProvenanceMainloopBuilt = "mainloop_built"

	SourcePrebuiltImage  = "prebuilt_image"
	SourceDefinitionRepo = "definition_repo"
)

// Budgets are the elapsed-time limits on registry work. Read applies to one registry read (token
// exchange, retry and CDN hop together); the others cover a whole validation or refresh.
type Budgets struct {
	Read       time.Duration
	Validation time.Duration
	Refresh    time.Duration
}

// DefaultBudgets are the production limits.
func DefaultBudgets() Budgets {
	return Budgets{Read: 30 * time.Second, Validation: 90 * time.Second, Refresh: 90 * time.Second}
}

// RegistryClient reads one metadata object. kind is "manifests" or "blobs"; reference is a digest,
// or a tag for a manifest. Implementations return *RegistryError for anything the caller should
// see as a 422, and the context's error when the caller's context ends.
type RegistryClient interface {
	Read(ctx context.Context, registry, repository, kind, reference string) ([]byte, error)
}

// RegistryError is an unsupported, inaccessible or invalid registry object. Its message is safe
// to show to the owner: it never contains a registry URL.
type RegistryError struct{ Message string }

func (e *RegistryError) Error() string { return e.Message }

func regErr(format string, args ...any) *RegistryError {
	return &RegistryError{Message: fmt.Sprintf(format, args...)}
}

var (
	referencePattern = regexp.MustCompile(`^([a-z0-9.-]+)/([a-z0-9._/-]+)@(sha256:[0-9a-f]{64})$`)
	digestPattern    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Reference is a parsed, allowlisted image reference.
type Reference struct{ Registry, Repository, Digest string }

// ParseReference accepts only registry/repository@sha256:digest on an allowlisted registry. A tag
// is a mutable name and is rejected here, before anything is read.
func ParseReference(reference string, allowlist []string) (Reference, error) {
	m := referencePattern.FindStringSubmatch(reference)
	if m == nil || slices.ContainsFunc(strings.Split(m[2], "/"), func(s string) bool {
		return s == "" || s == "." || s == ".."
	}) {
		return Reference{}, regErr("Register a registry/repository@sha256 digest reference, not a mutable tag")
	}
	if !slices.Contains(allowlist, m[1]) {
		return Reference{}, regErr("Registry %s is not allowlisted", m[1])
	}
	return Reference{Registry: m[1], Repository: m[2], Digest: m[3]}, nil
}

// Validator statically validates image references through a [RegistryClient].
type Validator struct {
	Client    RegistryClient
	Allowlist []string
	Budgets   Budgets
}

// withBudget runs fn under a deadline. When the deadline (and not the caller's context) ends the
// work, it reports the budget error in place of the raw context error.
func withBudget[T any](ctx context.Context, d time.Duration, operation string, fn func(context.Context) (T, error)) (T, error) {
	bctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	v, err := fn(bctx)
	if err != nil {
		var re *RegistryError
		if !errors.As(err, &re) && ctx.Err() == nil && bctx.Err() != nil {
			var zero T
			return zero, regErr("Registry %s elapsed-time budget exceeded", operation)
		}
	}
	return v, err
}

// Validate reads the image's manifests and config for the architecture and returns a
// static_validated version, ready to insert. It reads nothing when the reference is rejected.
func (v *Validator) Validate(ctx context.Context, reference, architecture, environmentID, versionID string) (dbq.InsertEnvironmentVersionParams, error) {
	return withBudget(ctx, v.Budgets.Validation, "validation", func(ctx context.Context) (dbq.InsertEnvironmentVersionParams, error) {
		return v.validate(ctx, reference, architecture, environmentID, versionID)
	})
}

func (v *Validator) read(ctx context.Context, ref Reference, kind, reference string) ([]byte, error) {
	data, err := v.Client.Read(ctx, ref.Registry, ref.Repository, kind, reference)
	if err != nil {
		var re *RegistryError
		if errors.As(err, &re) || ctx.Err() != nil {
			return nil, err
		}
		// A client's own error text may carry a request URL; the owner sees only this.
		return nil, regErr("Registry metadata unavailable or malformed")
	}
	return data, nil
}

func (v *Validator) validate(ctx context.Context, reference, architecture, environmentID, versionID string) (dbq.InsertEnvironmentVersionParams, error) {
	var zero dbq.InsertEnvironmentVersionParams
	if architecture != "amd64" && architecture != "arm64" {
		return zero, regErr("Unsupported architecture")
	}
	ref, err := ParseReference(reference, v.Allowlist)
	if err != nil {
		return zero, err
	}
	raw, err := v.read(ctx, ref, "manifests", ref.Digest)
	if err != nil {
		return zero, err
	}
	manifest, err := parseManifest(raw, ref.Digest)
	if err != nil {
		return zero, err
	}
	if manifest.SchemaVersion != 2 {
		return zero, regErr("Unsupported image manifest schema")
	}
	var indexDigest *string
	platformDigest := ref.Digest
	if manifest.Manifests != nil {
		indexDigest = &ref.Digest
		var matches []descriptor
		for _, d := range *manifest.Manifests {
			if d.Platform["os"] == "linux" && d.Platform["architecture"] == architecture {
				matches = append(matches, d)
			}
		}
		if len(matches) != 1 {
			return zero, regErr("Expected one linux/%s platform manifest", architecture)
		}
		platformDigest = matches[0].Digest
		raw, err = v.read(ctx, ref, "manifests", platformDigest)
		if err != nil {
			return zero, err
		}
		if int64(len(raw)) != matches[0].Size {
			return zero, regErr("Platform manifest size mismatch")
		}
		if manifest, err = parseManifest(raw, platformDigest); err != nil {
			return zero, err
		}
	}
	if manifest.SchemaVersion != 2 || manifest.Config == nil || manifest.Manifests != nil {
		return zero, regErr("Expected an image platform manifest with config")
	}
	configRaw, err := v.read(ctx, ref, "blobs", manifest.Config.Digest)
	if err != nil {
		return zero, err
	}
	if int64(len(configRaw)) != manifest.Config.Size {
		return zero, regErr("Config size mismatch")
	}
	cfg, err := parseImageConfig(configRaw, manifest.Config.Digest)
	if err != nil {
		return zero, err
	}
	if cfg.User == nil || *cfg.User != RequiredUser {
		return zero, regErr("Declared USER must be exactly %s; got %s", RequiredUser, describeUser(cfg.User))
	}
	if cfg.OS != "linux" || cfg.Architecture != architecture {
		return zero, regErr("Config platform does not match selected linux architecture")
	}
	result, err := json.Marshal(map[string]string{
		"user": RequiredUser, "platform": "linux/" + architecture, "probes": "not_run",
	})
	if err != nil {
		return zero, err
	}
	user := RequiredUser
	return dbq.InsertEnvironmentVersionParams{
		ID:                     versionID,
		EnvironmentID:          environmentID,
		Registry:               &ref.Registry,
		Repository:             &ref.Repository,
		IndexDigest:            indexDigest,
		PlatformManifestDigest: &platformDigest,
		ConfigDigest:           &manifest.Config.Digest,
		DeclaredUser:           &user,
		Architecture:           &architecture,
		CapabilityProfile:      json.RawMessage(`{}`),
		ValidationStatus:       StatusStaticValidated,
		ValidationResult:       result,
		ValidatorVersion:       ValidatorVersion,
		ProvenanceKind:         ProvenanceUserPushed,
		BaseProvenance:         "unknown",
	}, nil
}

// describeUser quotes a registry-supplied USER for the error message, bounded in length.
func describeUser(user *string) string {
	if user == nil {
		return "none"
	}
	s := *user
	if r := []rune(s); len(r) > 64 {
		s = string(r[:64]) + "…"
	}
	return fmt.Sprintf("%q", s)
}

// Refresh resolves the environment's watched tag to a new manifest digest and statically
// validates that image, returning a new version. It does not change the accepted default or any
// selection: the owner accepts a version explicitly.
func (v *Validator) Refresh(ctx context.Context, env dbq.DevEnvironment, previous dbq.EnvironmentVersion, versionID string) (dbq.InsertEnvironmentVersionParams, error) {
	return withBudget(ctx, v.Budgets.Refresh, "refresh", func(ctx context.Context) (dbq.InsertEnvironmentVersionParams, error) {
		var zero dbq.InsertEnvironmentVersionParams
		if env.WatchedTag == nil || *env.WatchedTag == "" || previous.Registry == nil || previous.Repository == nil || previous.PlatformManifestDigest == nil || previous.Architecture == nil {
			return zero, regErr("Environment has no watched image tag")
		}
		// Check the registry is still allowlisted before resolving any mutable metadata.
		ref, err := ParseReference(*previous.Registry+"/"+*previous.Repository+"@"+*previous.PlatformManifestDigest, v.Allowlist)
		if err != nil {
			return zero, err
		}
		raw, err := v.read(ctx, ref, "manifests", *env.WatchedTag)
		if err != nil {
			return zero, err
		}
		sum := sha256.Sum256(raw)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		return v.validate(ctx, ref.Registry+"/"+ref.Repository+"@"+digest, *previous.Architecture, env.ID, versionID)
	})
}

// descriptor is an OCI content descriptor, as far as validation reads it.
type descriptor struct {
	Digest   string
	Size     int64
	Platform map[string]string
}

type manifest struct {
	SchemaVersion int64
	Config        *descriptor
	Manifests     *[]descriptor
}

type imageConfig struct {
	Architecture string
	OS           string
	User         *string
}

var errInvalidMetadata = regErr("Invalid OCI metadata")

// checkedObject enforces the size cap and the content digest, then parses a JSON object. Keys
// match exactly: the standard decoder would accept "Config" for "config".
func checkedObject(data []byte, digest string) (map[string]json.RawMessage, error) {
	if len(data) > MaxMetadataBytes {
		return nil, regErr("Registry metadata exceeds size limit")
	}
	if sum := sha256.Sum256(data); "sha256:"+hex.EncodeToString(sum[:]) != digest {
		return nil, regErr("Registry content digest mismatch")
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, regErr("Invalid registry JSON")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		return nil, errInvalidMetadata
	}
	return obj, nil
}

func present(raw json.RawMessage, ok bool) bool {
	return ok && string(raw) != "null"
}

func parseManifest(data []byte, digest string) (manifest, error) {
	obj, err := checkedObject(data, digest)
	if err != nil {
		return manifest{}, err
	}
	var m manifest
	raw, ok := obj["schemaVersion"]
	if !ok || json.Unmarshal(raw, &m.SchemaVersion) != nil {
		return manifest{}, errInvalidMetadata
	}
	if raw, ok := obj["config"]; present(raw, ok) {
		d, err := parseDescriptor(raw)
		if err != nil {
			return manifest{}, err
		}
		m.Config = &d
	}
	if raw, ok := obj["manifests"]; present(raw, ok) {
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return manifest{}, errInvalidMetadata
		}
		list := make([]descriptor, 0, len(items))
		for _, item := range items {
			d, err := parseDescriptor(item)
			if err != nil {
				return manifest{}, err
			}
			list = append(list, d)
		}
		m.Manifests = &list
	}
	return m, nil
}

func parseDescriptor(data json.RawMessage) (descriptor, error) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil || obj == nil {
		return descriptor{}, errInvalidMetadata
	}
	var d descriptor
	raw, ok := obj["digest"]
	if !ok || json.Unmarshal(raw, &d.Digest) != nil || !digestPattern.MatchString(d.Digest) {
		return descriptor{}, errInvalidMetadata
	}
	raw, ok = obj["size"]
	if !ok || json.Unmarshal(raw, &d.Size) != nil || d.Size < 0 || d.Size > MaxMetadataBytes {
		return descriptor{}, errInvalidMetadata
	}
	if raw, ok := obj["platform"]; ok {
		if json.Unmarshal(raw, &d.Platform) != nil || d.Platform == nil {
			return descriptor{}, errInvalidMetadata
		}
	}
	return d, nil
}

func parseImageConfig(data []byte, digest string) (imageConfig, error) {
	obj, err := checkedObject(data, digest)
	if err != nil {
		return imageConfig{}, err
	}
	var c imageConfig
	raw, ok := obj["architecture"]
	if !ok || json.Unmarshal(raw, &c.Architecture) != nil || (c.Architecture != "amd64" && c.Architecture != "arm64") {
		return imageConfig{}, errInvalidMetadata
	}
	if raw, ok = obj["os"]; !ok || json.Unmarshal(raw, &c.OS) != nil {
		return imageConfig{}, errInvalidMetadata
	}
	if raw, ok := obj["config"]; ok {
		var inner map[string]json.RawMessage
		if json.Unmarshal(raw, &inner) != nil || inner == nil {
			return imageConfig{}, errInvalidMetadata
		}
		if rawUser, ok := inner["User"]; ok && string(rawUser) != "null" {
			var user string
			if json.Unmarshal(rawUser, &user) != nil {
				// A non-string USER can never equal the required one; report it as unset.
				return c, nil
			}
			c.User = &user
		}
	}
	return c, nil
}
