// Package fakeregistry is an in-memory OCI registry for tests. A missing object never falls
// through to the network: it reads as a private or absent image.
package fakeregistry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"

	"github.com/oldsj/mainloop/server/internal/environments"
)

// Key addresses one stored object.
type Key struct{ Registry, Repository, Kind, Reference string }

// Registry implements [environments.RegistryClient].
type Registry struct {
	mu      sync.Mutex
	objects map[Key][]byte
	calls   []Key
}

// New returns an empty registry.
func New() *Registry { return &Registry{objects: map[Key][]byte{}} }

// Read records the call and returns the stored object.
func (r *Registry) Read(_ context.Context, registry, repository, kind, reference string) ([]byte, error) {
	k := Key{registry, repository, kind, reference}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, k)
	data, ok := r.objects[k]
	if !ok {
		return nil, &environments.RegistryError{Message: "private images not supported yet"}
	}
	return data, nil
}

// Put stores or replaces an object.
func (r *Registry) Put(registry, repository, kind, reference string, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.objects[Key{registry, repository, kind, reference}] = data
}

// Get returns a stored object.
func (r *Registry) Get(registry, repository, kind, reference string) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.objects[Key{registry, repository, kind, reference}]
}

// Calls returns every Read so far, in order.
func (r *Registry) Calls() []Key {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Key(nil), r.calls...)
}

// Objects returns a copy of the stored objects, for serving over HTTP.
func (r *Registry) Objects() map[Key][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[Key][]byte, len(r.objects))
	for k, v := range r.objects {
		out[k] = v
	}
	return out
}

// Digest returns the sha256 digest of data in OCI form.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Options shapes the image AddImage stores. The zero value is a valid single-platform
// linux/amd64 image whose USER is 65532:65532.
type Options struct {
	// Arch is the config architecture (default amd64).
	Arch string
	// OS is the config operating system (default linux).
	OS string
	// User is the config USER (default 65532:65532). NoUser omits the key.
	User   string
	NoUser bool
	// Label, when set, goes into the config so images with different labels have different digests.
	Label string
	// Index stores a two-platform index (amd64 and arm64) and returns it as the top object.
	Index bool
}

// Image is what AddImage stored.
type Image struct {
	Registry, Repository string
	// Top is the digest of the index, or of the manifest when there is no index.
	Top string
	// Index is the index digest, empty for a single manifest.
	Index string
	// Manifest is the selected platform manifest digest; Config is its config blob digest.
	Manifest, Config string
}

// Reference is registry/repository@top, the form the API accepts.
func (i Image) Reference() string { return i.Registry + "/" + i.Repository + "@" + i.Top }

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// AddImage stores an image's manifests and config blob (never layers) and returns its digests.
func (r *Registry) AddImage(registry, repository string, o Options) Image {
	if o.Arch == "" {
		o.Arch = "amd64"
	}
	if o.OS == "" {
		o.OS = "linux"
	}
	if o.User == "" && !o.NoUser {
		o.User = "65532:65532"
	}
	img := Image{Registry: registry, Repository: repository}
	store := func(kind string, data []byte) string {
		d := Digest(data)
		r.Put(registry, repository, kind, d, data)
		return d
	}
	build := func(arch string, withUser bool) (manifest, config string, size int64) {
		cfg := map[string]any{"architecture": arch, "os": o.OS}
		if withUser {
			cfg["config"] = map[string]any{"User": o.User}
			if o.Label != "" {
				cfg["config"] = map[string]any{"User": o.User, "Labels": map[string]string{"label": o.Label}}
			}
		} else {
			cfg["config"] = map[string]any{}
		}
		raw := mustJSON(cfg)
		config = store("blobs", raw)
		m := mustJSON(map[string]any{
			"schemaVersion": 2,
			"mediaType":     "application/vnd.oci.image.manifest.v1+json",
			"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": config, "size": len(raw)},
			// A layer that is never read: it is not stored, so reading it would fail the test.
			"layers": []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": Digest([]byte("layer" + arch)), "size": 123}},
		})
		return store("manifests", m), config, int64(len(m))
	}
	selected, selectedConfig, selectedSize := build(o.Arch, !o.NoUser)
	img.Manifest, img.Config, img.Top = selected, selectedConfig, selected
	if !o.Index {
		return img
	}
	other := "arm64"
	if o.Arch == "arm64" {
		other = "amd64"
	}
	otherDigest, _, otherSize := build(other, true)
	entry := func(digest string, size int64, arch string) map[string]any {
		return map[string]any{
			"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": digest, "size": size,
			"platform": map[string]any{"os": "linux", "architecture": arch},
		}
	}
	index := mustJSON(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests":     []any{entry(selected, selectedSize, o.Arch), entry(otherDigest, otherSize, other)},
	})
	img.Index = store("manifests", index)
	img.Top = img.Index
	return img
}
