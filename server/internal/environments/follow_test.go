package environments_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/environments/fakeregistry"
)

// moveTag points the watched tag at a new image and returns its platform manifest digest.
func (f *fixture) moveTag(repo, tag string) string {
	f.t.Helper()
	f.moves++
	next := f.registry.AddImage("ghcr.io", repo, fakeregistry.Options{Index: true, Arch: "arm64", Label: fmt.Sprint("move", f.moves)})
	f.registry.Put("ghcr.io", repo, "manifests", tag, f.registry.Get("ghcr.io", repo, "manifests", next.Index))
	return next.Manifest
}

func (f *fixture) versions(envID string) []map[string]any {
	f.t.Helper()
	return f.want(f.do("GET", "/environments/"+envID+"/versions", nil), 200).list()
}

func (f *fixture) defaultVersion(envID string) any {
	f.t.Helper()
	return f.want(f.do("GET", "/environments/"+envID, nil), 200).json()["accepted_default_version_id"]
}

func TestFollowRequiresWatchedTag(t *testing.T) {
	f := newFixture(t)
	img := f.registry.AddImage("ghcr.io", "oldsj/dev", fakeregistry.Options{})
	r := f.want(f.do("POST", "/environments", map[string]any{"name": "dev", "image": img.Reference(), "follow_watched_tag": true}), 422)
	if r.detail() == "" {
		t.Errorf("no detail: %s", r.raw)
	}
	if n := f.count("dev_environments"); n != 0 {
		t.Errorf("rejected registration stored %d environments", n)
	}
}

func TestFollowAcceptsNewImagesFromTheWatchedTag(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	envID, v1 := f.register("dev", fakeregistry.Options{Arch: "arm64"}, map[string]any{
		"watched_tag": "cache-main", "follow_watched_tag": true, "architecture": "arm64",
	})
	if env := f.want(f.do("GET", "/environments/"+envID, nil), 200).json(); env["follow_watched_tag"] != true {
		t.Fatalf("environment = %v", env)
	}
	// An environment that did not opt in is never touched.
	otherID, other := f.register("manual", fakeregistry.Options{Arch: "arm64"}, map[string]any{"watched_tag": "stable", "architecture": "arm64"})
	f.moveTag("oldsj/manual", "stable")

	// The tag has to exist for the first pass. It names the registered image, so nothing is
	// appended and the first version becomes the default.
	img := f.registry.Get("ghcr.io", "oldsj/dev", "manifests", f.versions(envID)[0]["platform_manifest_digest"].(string))
	f.registry.Put("ghcr.io", "oldsj/dev", "manifests", "cache-main", img)
	if err := f.service.Follow(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if got := f.defaultVersion(envID); got != v1 {
		t.Errorf("default = %v, want the first version %v", got, v1)
	}
	if n := len(f.versions(envID)); n != 1 {
		t.Errorf("an unchanged tag appended a version: %d versions", n)
	}

	// A pass that finds nothing new stays quiet.
	if err := f.service.Follow(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if n := len(f.versions(envID)); n != 1 {
		t.Errorf("repeat pass appended a version: %d versions", n)
	}

	// The tag moves: one new version, accepted as the default.
	digest := f.moveTag("oldsj/dev", "cache-main")
	if err := f.service.Follow(ctx, owner); err != nil {
		t.Fatal(err)
	}
	versions := f.versions(envID)
	if len(versions) != 2 || versions[1]["platform_manifest_digest"] != digest || versions[1]["validation_status"] != "static_validated" {
		t.Fatalf("versions = %v", versions)
	}
	if got := f.defaultVersion(envID); got != versions[1]["id"] {
		t.Errorf("default = %v, want %v", got, versions[1]["id"])
	}
	if err := f.service.Follow(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if n := len(f.versions(envID)); n != 2 {
		t.Errorf("repeat pass appended a version: %d versions", n)
	}

	// A project that follows the default resolves to the new version; one pinned to the first
	// version keeps it.
	f.project("p-follow", owner)
	f.project("p-pinned", owner)
	f.want(f.do("PUT", "/projects/p-follow/environment", map[string]any{"environment_id": envID, "follow_default": true, "expected_version": 0}), 200)
	f.want(f.do("PUT", "/projects/p-pinned/environment", map[string]any{"environment_id": envID, "version_id": v1, "expected_version": 0}), 200)
	if got := f.want(f.do("GET", "/projects/p-follow/environment", nil), 200).json()["resolved_version_id"]; got != versions[1]["id"] {
		t.Errorf("following project resolves to %v, want %v", got, versions[1]["id"])
	}
	if got := f.want(f.do("GET", "/projects/p-pinned/environment", nil), 200).json()["resolved_version_id"]; got != v1 {
		t.Errorf("pinned project resolves to %v, want %v", got, v1)
	}

	if f.defaultVersion(otherID) != nil || len(f.versions(otherID)) != 1 || f.versions(otherID)[0]["id"] != other {
		t.Errorf("environment without follow_watched_tag changed")
	}
}

func TestFollowFailureLeavesStateAndDoesNotStopOthers(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	badID, bad := f.register("bad", fakeregistry.Options{Arch: "arm64"}, map[string]any{"watched_tag": "cache-main", "follow_watched_tag": true, "architecture": "arm64"})
	goodID, _ := f.register("good", fakeregistry.Options{Arch: "arm64"}, map[string]any{"watched_tag": "cache-main", "follow_watched_tag": true, "architecture": "arm64"})
	// Neither tag resolves yet. The bad one stays broken; the good one gets an image.
	f.registry.Put("ghcr.io", "oldsj/bad", "manifests", "cache-main", []byte(`{"schemaVersion":1}`))
	digest := f.moveTag("oldsj/good", "cache-main")

	if err := f.service.Follow(ctx, owner); err == nil {
		t.Error("a tag that does not resolve reported no error")
	}
	if f.defaultVersion(badID) != nil || len(f.versions(badID)) != 1 || f.versions(badID)[0]["id"] != bad {
		t.Errorf("failed environment changed: default %v, %d versions", f.defaultVersion(badID), len(f.versions(badID)))
	}
	if versions := f.versions(goodID); len(versions) != 2 || versions[1]["platform_manifest_digest"] != digest || f.defaultVersion(goodID) != versions[1]["id"] {
		t.Errorf("good environment was not followed: %v", versions)
	}
}

func TestFollowStopsWhenOptedOut(t *testing.T) {
	f := newFixture(t)
	envID, _ := f.register("dev", fakeregistry.Options{Arch: "arm64"}, map[string]any{"watched_tag": "cache-main", "follow_watched_tag": true, "architecture": "arm64"})
	f.moveTag("oldsj/dev", "cache-main")
	if _, err := f.db.Pool.Exec(context.Background(), `UPDATE dev_environments SET follow_watched_tag = false WHERE id = $1`, envID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Follow(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	if n := len(f.versions(envID)); n != 1 || f.defaultVersion(envID) != nil {
		t.Errorf("opted-out environment was followed: %d versions, default %v", n, f.defaultVersion(envID))
	}
}

// clock is a settable time source for the follow interval.
type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func TestFollowAcceptsAtMostOneImagePerInterval(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := &clock{now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	f.service.Now = c.Now
	f.service.FollowInterval = time.Hour
	envID, v1 := f.register("dev", fakeregistry.Options{Arch: "arm64"}, map[string]any{
		"watched_tag": "cache-main", "follow_watched_tag": true, "architecture": "arm64",
	})
	img := f.registry.Get("ghcr.io", "oldsj/dev", "manifests", f.versions(envID)[0]["platform_manifest_digest"].(string))
	f.registry.Put("ghcr.io", "oldsj/dev", "manifests", "cache-main", img)
	pass := func() {
		t.Helper()
		if err := f.service.Follow(ctx, owner); err != nil {
			t.Fatal(err)
		}
	}
	pass()
	if got := f.defaultVersion(envID); got != v1 {
		t.Fatalf("first default = %v, want %v", got, v1)
	}

	// The first new image is accepted at once, however recently the environment was registered.
	f.moveTag("oldsj/dev", "cache-main")
	pass()
	versions := f.versions(envID)
	if len(versions) != 2 || f.defaultVersion(envID) != versions[1]["id"] {
		t.Fatalf("first change not accepted: %d versions, default %v", len(versions), f.defaultVersion(envID))
	}

	// Further moves inside the interval are left for later, and cost no registry request.
	f.moveTag("oldsj/dev", "cache-main")
	c.now = c.now.Add(59 * time.Minute)
	pass()
	if n := len(f.versions(envID)); n != 2 || f.defaultVersion(envID) != versions[1]["id"] {
		t.Fatalf("change inside the interval accepted: %d versions", n)
	}

	// Once the interval has passed the newest image is accepted; the one skipped is never minted.
	c.now = c.now.Add(2 * time.Minute)
	pass()
	after := f.versions(envID)
	if len(after) != 3 || f.defaultVersion(envID) != after[2]["id"] {
		t.Fatalf("change after the interval not accepted: %d versions", len(after))
	}
	pass()
	if n := len(f.versions(envID)); n != 3 {
		t.Errorf("repeat pass appended a version: %d versions", n)
	}
}

func TestFollowNeverPromotesAnIneligibleVersion(t *testing.T) {
	f := newFixture(t)
	f.service.FollowInterval = 0
	ctx := context.Background()
	envID, v1 := f.register("dev", fakeregistry.Options{Arch: "arm64"}, map[string]any{
		"watched_tag": "cache-main", "follow_watched_tag": true, "architecture": "arm64",
	})
	img := f.registry.Get("ghcr.io", "oldsj/dev", "manifests", f.versions(envID)[0]["platform_manifest_digest"].(string))
	f.registry.Put("ghcr.io", "oldsj/dev", "manifests", "cache-main", img)

	// The newest version names the tag's image but was validated by an older policy. SetDefault
	// refuses it, so the sweep must too: it appends a fresh version instead.
	if _, err := f.db.Pool.Exec(ctx, `INSERT INTO environment_versions (id, environment_id, registry, repository, platform_manifest_digest, config_digest, declared_user, architecture, validation_status, validator_version, provenance_kind)
		SELECT 'stale', environment_id, registry, repository, platform_manifest_digest, config_digest, declared_user, architecture, validation_status, 'oci-static-v1', provenance_kind
		FROM environment_versions WHERE id = $1`, v1); err != nil {
		t.Fatal(err)
	}
	f.want(f.do("PUT", "/environments/"+envID+"/default", map[string]any{"version_id": "stale"}), 422)
	if err := f.service.Follow(ctx, owner); err != nil {
		t.Fatal(err)
	}
	versions := f.versions(envID)
	last := versions[len(versions)-1]
	if len(versions) != 3 || last["id"] == "stale" || last["validator_version"] != "oci-static-v2" || f.defaultVersion(envID) != last["id"] {
		t.Fatalf("stale version promoted: default %v, versions %v", f.defaultVersion(envID), versions)
	}
}

func TestUpdateEnvironmentTurnsFollowingOffAndOn(t *testing.T) {
	f := newFixture(t)
	f.service.FollowInterval = 0
	ctx := context.Background()
	envID, _ := f.register("dev", fakeregistry.Options{Arch: "arm64"}, map[string]any{
		"watched_tag": "cache-main", "follow_watched_tag": true, "architecture": "arm64",
	})
	plainID, _ := f.register("plain", fakeregistry.Options{Arch: "arm64"}, map[string]any{"architecture": "arm64"})
	patch := func(id string, follow bool) response {
		return f.do("PATCH", "/environments/"+id, map[string]any{"follow_watched_tag": follow})
	}

	if got := f.want(patch(envID, false), 200).json(); got["follow_watched_tag"] != false || got["watched_tag"] != "cache-main" {
		t.Fatalf("environment = %v", got)
	}
	f.moveTag("oldsj/dev", "cache-main")
	if err := f.service.Follow(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if n := len(f.versions(envID)); n != 1 || f.defaultVersion(envID) != nil {
		t.Errorf("environment kept following: %d versions, default %v", n, f.defaultVersion(envID))
	}
	if got := f.want(patch(envID, true), 200).json(); got["follow_watched_tag"] != true {
		t.Fatalf("environment = %v", got)
	}
	if err := f.service.Follow(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if n := len(f.versions(envID)); n != 2 {
		t.Errorf("environment did not resume following: %d versions", n)
	}

	f.want(patch(plainID, true), 422)
	f.want(patch(plainID, false), 200)
	f.want(patch("missing", false), 404)
	f.want(f.do("PATCH", "/environments/"+envID, map[string]any{}), 422)
}
