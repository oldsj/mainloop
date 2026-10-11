package environments_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/environments"
	"github.com/oldsj/mainloop/server/internal/environments/fakeregistry"
)

const reg = "ghcr.io"

func validator(c environments.RegistryClient) *environments.Validator {
	return &environments.Validator{Client: c, Allowlist: []string{reg}, Budgets: environments.DefaultBudgets()}
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func wantRegistryError(t *testing.T, err error, contains string) {
	t.Helper()
	var re *environments.RegistryError
	if !errors.As(err, &re) {
		t.Fatalf("error = %v (%T), want a RegistryError containing %q", err, err, contains)
	}
	if !strings.Contains(re.Message, contains) {
		t.Fatalf("error = %q, want it to contain %q", re.Message, contains)
	}
}

func TestValidateSingleManifest(t *testing.T) {
	r := fakeregistry.New()
	img := r.AddImage(reg, "oldsj/dev", fakeregistry.Options{})
	v, err := validator(r).Validate(context.Background(), img.Reference(), "amd64", "env1", "ver1")
	if err != nil {
		t.Fatal(err)
	}
	if v.ValidationStatus != environments.StatusStaticValidated || v.ValidatorVersion != "oci-static-v2" ||
		v.ProvenanceKind != environments.ProvenanceUserPushed || v.BaseProvenance != "unknown" {
		t.Errorf("status fields = %+v", v)
	}
	if v.IndexDigest != nil || str(v.PlatformManifestDigest) != img.Manifest || str(v.ConfigDigest) != img.Config ||
		str(v.Registry) != reg || str(v.Repository) != "oldsj/dev" || str(v.DeclaredUser) != "65532:65532" || str(v.Architecture) != "amd64" {
		t.Errorf("evidence = %+v", v)
	}
	var result map[string]string
	if err := json.Unmarshal(v.ValidationResult, &result); err != nil {
		t.Fatal(err)
	}
	if result["user"] != "65532:65532" || result["platform"] != "linux/amd64" || result["probes"] != "not_run" {
		t.Errorf("result = %v", result)
	}
	// Metadata only: the manifest and the config blob, never the layer.
	want := []fakeregistry.Key{
		{Registry: reg, Repository: "oldsj/dev", Kind: "manifests", Reference: img.Manifest},
		{Registry: reg, Repository: "oldsj/dev", Kind: "blobs", Reference: img.Config},
	}
	got := r.Calls()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestValidateIndexSelectsRequestedPlatform(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			r := fakeregistry.New()
			img := r.AddImage(reg, "oldsj/dev", fakeregistry.Options{Arch: arch, Index: true})
			v, err := validator(r).Validate(context.Background(), img.Reference(), arch, "e", "v")
			if err != nil {
				t.Fatal(err)
			}
			if str(v.IndexDigest) != img.Index || str(v.PlatformManifestDigest) != img.Manifest || str(v.Architecture) != arch {
				t.Errorf("version = index %s manifest %s arch %s; want %s %s %s",
					str(v.IndexDigest), str(v.PlatformManifestDigest), str(v.Architecture), img.Index, img.Manifest, arch)
			}
			if n := len(r.Calls()); n != 3 {
				t.Errorf("calls = %d, want 3 (index, manifest, config)", n)
			}
		})
	}
}

func TestValidateRejectsWrongUser(t *testing.T) {
	for _, user := range []string{"0:0", "root", "1000:1000", "nonroot", "65532", "65532:65532 ", "65532:0"} {
		t.Run(user, func(t *testing.T) {
			r := fakeregistry.New()
			img := r.AddImage(reg, "oldsj/dev", fakeregistry.Options{User: user})
			_, err := validator(r).Validate(context.Background(), img.Reference(), "amd64", "e", "v")
			wantRegistryError(t, err, "Declared USER must be exactly 65532:65532")
		})
	}
	t.Run("missing", func(t *testing.T) {
		r := fakeregistry.New()
		img := r.AddImage(reg, "oldsj/dev", fakeregistry.Options{NoUser: true})
		_, err := validator(r).Validate(context.Background(), img.Reference(), "amd64", "e", "v")
		wantRegistryError(t, err, "got none")
	})
}

func TestValidateRejectsBeforeAnyRegistryCall(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for name, ref := range map[string]string{
		"registry not allowlisted": "docker.io/oldsj/dev@" + digest,
		"mutable tag":              "ghcr.io/oldsj/dev:latest",
		"empty segment":            "ghcr.io/oldsj//dev@" + digest,
		"dot segment":              "ghcr.io/oldsj/./dev@" + digest,
		"dotdot segment":           "ghcr.io/oldsj/../dev@" + digest,
		"uppercase":                "ghcr.io/Oldsj/dev@" + digest,
		"short digest":             "ghcr.io/oldsj/dev@sha256:abc",
		"sha512":                   "ghcr.io/oldsj/dev@sha512:" + strings.Repeat("a", 64),
		"no repository":            "ghcr.io@" + digest,
		"url form":                 "https://ghcr.io/oldsj/dev@" + digest,
		"empty":                    "",
	} {
		t.Run(name, func(t *testing.T) {
			r := fakeregistry.New()
			_, err := validator(r).Validate(context.Background(), ref, "amd64", "e", "v")
			wantRegistryError(t, err, "")
			if n := len(r.Calls()); n != 0 {
				t.Errorf("registry calls = %d, want 0", n)
			}
		})
	}
	r := fakeregistry.New()
	_, err := validator(r).Validate(context.Background(), "docker.io/oldsj/dev@"+digest, "amd64", "e", "v")
	wantRegistryError(t, err, "Registry docker.io is not allowlisted")
}

func TestValidateRejectsBadMetadata(t *testing.T) {
	ctx := context.Background()
	t.Run("config size differs from the manifest", func(t *testing.T) {
		r := fakeregistry.New()
		img := r.AddImage(reg, "oldsj/dev", fakeregistry.Options{})
		r.Put(reg, "oldsj/dev", "blobs", img.Config, append(r.Get(reg, "oldsj/dev", "blobs", img.Config), ' '))
		_, err := validator(r).Validate(ctx, img.Reference(), "amd64", "e", "v")
		wantRegistryError(t, err, "Config size mismatch")
	})
	t.Run("config digest mismatch", func(t *testing.T) {
		r := fakeregistry.New()
		img := r.AddImage(reg, "oldsj/dev", fakeregistry.Options{})
		orig := r.Get(reg, "oldsj/dev", "blobs", img.Config)
		tampered := []byte(strings.Replace(string(orig), "amd64", "arm64", 1))
		if len(tampered) != len(orig) {
			t.Fatal("fixture: tampering changed the length")
		}
		r.Put(reg, "oldsj/dev", "blobs", img.Config, tampered)
		_, err := validator(r).Validate(ctx, img.Reference(), "amd64", "e", "v")
		wantRegistryError(t, err, "digest mismatch")
	})
	t.Run("manifest bytes do not match digest", func(t *testing.T) {
		r := fakeregistry.New()
		img := r.AddImage(reg, "oldsj/dev", fakeregistry.Options{})
		r.Put(reg, "oldsj/dev", "manifests", img.Top, []byte(`{"schemaVersion":2}`))
		_, err := validator(r).Validate(ctx, img.Reference(), "amd64", "e", "v")
		wantRegistryError(t, err, "digest mismatch")
	})
	t.Run("config architecture differs from request", func(t *testing.T) {
		r := fakeregistry.New()
		img := r.AddImage(reg, "oldsj/dev", fakeregistry.Options{Arch: "arm64"})
		_, err := validator(r).Validate(ctx, img.Reference(), "amd64", "e", "v")
		wantRegistryError(t, err, "Config platform does not match")
	})
	t.Run("config os is not linux", func(t *testing.T) {
		r := fakeregistry.New()
		img := r.AddImage(reg, "oldsj/dev", fakeregistry.Options{OS: "windows"})
		_, err := validator(r).Validate(ctx, img.Reference(), "amd64", "e", "v")
		wantRegistryError(t, err, "Config platform does not match")
	})
	t.Run("unsupported architecture request", func(t *testing.T) {
		r := fakeregistry.New()
		img := r.AddImage(reg, "oldsj/dev", fakeregistry.Options{})
		_, err := validator(r).Validate(ctx, img.Reference(), "riscv64", "e", "v")
		wantRegistryError(t, err, "Unsupported architecture")
		if len(r.Calls()) != 0 {
			t.Error("read the registry for an unsupported architecture")
		}
	})
	t.Run("manifest schema version 1", func(t *testing.T) {
		r := fakeregistry.New()
		raw := []byte(`{"schemaVersion":1}`)
		d := fakeregistry.Digest(raw)
		r.Put(reg, "oldsj/dev", "manifests", d, raw)
		_, err := validator(r).Validate(ctx, reg+"/oldsj/dev@"+d, "amd64", "e", "v")
		wantRegistryError(t, err, "Unsupported image manifest schema")
	})
	t.Run("keys match case-sensitively", func(t *testing.T) {
		r := fakeregistry.New()
		raw := []byte(`{"schemaVersion":2,"Config":{"digest":"sha256:` + strings.Repeat("a", 64) + `","size":1}}`)
		d := fakeregistry.Digest(raw)
		r.Put(reg, "oldsj/dev", "manifests", d, raw)
		_, err := validator(r).Validate(ctx, reg+"/oldsj/dev@"+d, "amd64", "e", "v")
		wantRegistryError(t, err, "Expected an image platform manifest with config")
	})
	t.Run("invalid json", func(t *testing.T) {
		r := fakeregistry.New()
		raw := []byte(`not json`)
		d := fakeregistry.Digest(raw)
		r.Put(reg, "oldsj/dev", "manifests", d, raw)
		_, err := validator(r).Validate(ctx, reg+"/oldsj/dev@"+d, "amd64", "e", "v")
		wantRegistryError(t, err, "Invalid registry JSON")
	})
	t.Run("metadata over the cap", func(t *testing.T) {
		r := fakeregistry.New()
		raw := []byte(strings.Repeat(" ", environments.MaxMetadataBytes+1))
		d := fakeregistry.Digest(raw)
		r.Put(reg, "oldsj/dev", "manifests", d, raw)
		_, err := validator(r).Validate(ctx, reg+"/oldsj/dev@"+d, "amd64", "e", "v")
		wantRegistryError(t, err, "size limit")
	})
	t.Run("a registry client's raw error is not shown", func(t *testing.T) {
		_, err := validator(failing{errors.New("Get https://ghcr.io/v2/x?sig=secret: boom")}).Validate(ctx,
			reg+"/oldsj/dev@sha256:"+strings.Repeat("a", 64), "amd64", "e", "v")
		wantRegistryError(t, err, "Registry metadata unavailable or malformed")
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("error leaks the client's text: %v", err)
		}
	})
}

type failing struct{ err error }

func (f failing) Read(context.Context, string, string, string, string) ([]byte, error) {
	return nil, f.err
}

func TestValidateIndexPlatformChecks(t *testing.T) {
	ctx := context.Background()
	rewriteIndex := func(r *fakeregistry.Registry, img fakeregistry.Image, edit func(string) string) string {
		raw := edit(string(r.Get(reg, "oldsj/dev", "manifests", img.Index)))
		d := fakeregistry.Digest([]byte(raw))
		r.Put(reg, "oldsj/dev", "manifests", d, []byte(raw))
		return reg + "/oldsj/dev@" + d
	}
	t.Run("no matching platform", func(t *testing.T) {
		r := fakeregistry.New()
		img := r.AddImage(reg, "oldsj/dev", fakeregistry.Options{Index: true})
		ref := rewriteIndex(r, img, func(s string) string { return strings.ReplaceAll(s, `"architecture":"amd64"`, `"architecture":"s390x"`) })
		_, err := validator(r).Validate(ctx, ref, "amd64", "e", "v")
		wantRegistryError(t, err, "Expected one linux/amd64 platform manifest")
	})
	t.Run("two matching platforms", func(t *testing.T) {
		r := fakeregistry.New()
		img := r.AddImage(reg, "oldsj/dev", fakeregistry.Options{Index: true})
		ref := rewriteIndex(r, img, func(s string) string { return strings.ReplaceAll(s, `"architecture":"arm64"`, `"architecture":"amd64"`) })
		_, err := validator(r).Validate(ctx, ref, "amd64", "e", "v")
		wantRegistryError(t, err, "Expected one linux/amd64 platform manifest")
	})
	t.Run("platform manifest size differs from the index", func(t *testing.T) {
		r := fakeregistry.New()
		img := r.AddImage(reg, "oldsj/dev", fakeregistry.Options{Index: true})
		var idx struct {
			Manifests []struct{ Size int } `json:"manifests"`
		}
		if err := json.Unmarshal(r.Get(reg, "oldsj/dev", "manifests", img.Index), &idx); err != nil {
			t.Fatal(err)
		}
		size := idx.Manifests[0].Size
		ref := rewriteIndex(r, img, func(s string) string {
			return strings.Replace(s, `"size":`+itoa(size), `"size":`+itoa(size+1), 1)
		})
		_, err := validator(r).Validate(ctx, ref, "amd64", "e", "v")
		wantRegistryError(t, err, "Platform manifest size mismatch")
	})
	t.Run("index nested in an index", func(t *testing.T) {
		r := fakeregistry.New()
		img := r.AddImage(reg, "oldsj/dev", fakeregistry.Options{Index: true})
		// Point the amd64 entry at the index itself: the "platform manifest" has no config.
		var idx struct {
			Manifests []struct {
				Digest string
				Size   int
			} `json:"manifests"`
		}
		raw := r.Get(reg, "oldsj/dev", "manifests", img.Index)
		if err := json.Unmarshal(raw, &idx); err != nil {
			t.Fatal(err)
		}
		r.Put(reg, "oldsj/dev", "manifests", idx.Manifests[0].Digest, raw)
		ref := rewriteIndex(r, img, func(s string) string {
			s = strings.Replace(s, idx.Manifests[0].Digest, fakeregistry.Digest(raw), 1)
			return strings.Replace(s, `"size":`+itoa(idx.Manifests[0].Size), `"size":`+itoa(len(raw)), 1)
		})
		r.Put(reg, "oldsj/dev", "manifests", fakeregistry.Digest(raw), raw)
		_, err := validator(r).Validate(ctx, ref, "amd64", "e", "v")
		if err == nil {
			t.Fatal("accepted a nested index")
		}
		wantRegistryError(t, err, "")
	})
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestRefresh(t *testing.T) {
	ctx := context.Background()
	r := fakeregistry.New()
	old := r.AddImage(reg, "oldsj/dev", fakeregistry.Options{Index: true})
	next := r.AddImage(reg, "oldsj/dev", fakeregistry.Options{Index: true, User: "65532:65532", Arch: "amd64"})
	// A different image under the watched tag: change the config so the digests differ.
	r.Put(reg, "oldsj/dev", "manifests", "stable", r.Get(reg, "oldsj/dev", "manifests", next.Index))
	tag := "stable"
	arch := "amd64"
	previous := dbq.EnvironmentVersion{
		ID: "old", EnvironmentID: "env", Registry: strp(reg), Repository: strp("oldsj/dev"),
		PlatformManifestDigest: strp(old.Manifest), Architecture: &arch,
	}
	env := dbq.DevEnvironment{ID: "env", WatchedTag: &tag}

	v, err := validator(r).Refresh(ctx, env, previous, "new")
	if err != nil {
		t.Fatal(err)
	}
	if v.ID != "new" || v.EnvironmentID != "env" || str(v.IndexDigest) != next.Index || str(v.PlatformManifestDigest) != next.Manifest {
		t.Errorf("refreshed version = %+v", v)
	}
	calls := r.Calls()
	if calls[0].Kind != "manifests" || calls[0].Reference != "stable" {
		t.Errorf("first call = %+v, want the watched tag", calls[0])
	}

	t.Run("no watched tag", func(t *testing.T) {
		_, err := validator(r).Refresh(ctx, dbq.DevEnvironment{ID: "env"}, previous, "x")
		wantRegistryError(t, err, "no watched image tag")
	})
	t.Run("pending build version", func(t *testing.T) {
		_, err := validator(r).Refresh(ctx, env, dbq.EnvironmentVersion{ID: "p", EnvironmentID: "env"}, "x")
		wantRegistryError(t, err, "no watched image tag")
	})
	t.Run("registry no longer allowlisted", func(t *testing.T) {
		fresh := fakeregistry.New()
		val := &environments.Validator{Client: fresh, Allowlist: []string{"registry.example"}, Budgets: environments.DefaultBudgets()}
		_, err := val.Refresh(ctx, env, previous, "x")
		wantRegistryError(t, err, "not allowlisted")
		if len(fresh.Calls()) != 0 {
			t.Error("resolved the tag on a registry that is no longer allowlisted")
		}
	})
	t.Run("tag missing", func(t *testing.T) {
		tag := "gone"
		_, err := validator(r).Refresh(ctx, dbq.DevEnvironment{ID: "env", WatchedTag: &tag}, previous, "x")
		wantRegistryError(t, err, "private images not supported yet")
	})
}

func strp(s string) *string { return &s }

// blocking waits for its context to end.
type blocking struct{}

func (blocking) Read(ctx context.Context, _, _, _, _ string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestBudgets(t *testing.T) {
	ref := reg + "/oldsj/dev@sha256:" + strings.Repeat("a", 64)
	tag := "stable"
	arch := "amd64"
	previous := dbq.EnvironmentVersion{
		ID: "old", EnvironmentID: "env", Registry: strp(reg), Repository: strp("oldsj/dev"),
		PlatformManifestDigest: strp("sha256:" + strings.Repeat("a", 64)), Architecture: &arch,
	}
	env := dbq.DevEnvironment{ID: "env", WatchedTag: &tag}
	small := environments.Budgets{Read: time.Hour, Validation: 20 * time.Millisecond, Refresh: 20 * time.Millisecond}
	val := &environments.Validator{Client: blocking{}, Allowlist: []string{reg}, Budgets: small}

	t.Run("validation", func(t *testing.T) {
		_, err := val.Validate(context.Background(), ref, "amd64", "e", "v")
		wantRegistryError(t, err, "Registry validation elapsed-time budget exceeded")
	})
	t.Run("refresh", func(t *testing.T) {
		_, err := val.Refresh(context.Background(), env, previous, "v")
		wantRegistryError(t, err, "Registry refresh elapsed-time budget exceeded")
	})
	t.Run("caller cancellation is preserved", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(10*time.Millisecond, cancel)
		_, err := val.Validate(ctx, ref, "amd64", "e", "v")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		var re *environments.RegistryError
		if errors.As(err, &re) {
			t.Errorf("cancellation was reported as a registry error: %v", err)
		}
	})
}
