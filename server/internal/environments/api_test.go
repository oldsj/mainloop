package environments_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/app"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/environments"
	"github.com/oldsj/mainloop/server/internal/environments/fakeregistry"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

const owner = "local-dev-user"

type fixture struct {
	t        *testing.T
	db       *testdb.DB
	handler  http.Handler
	registry *fakeregistry.Registry
	service  *environments.Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test"})
	if err != nil {
		t.Fatal(err)
	}
	d := testdb.New(t)
	reg := fakeregistry.New()
	h, err := app.New(app.Deps{Config: cfg, Log: slog.New(slog.NewJSONHandler(io.Discard, nil)), Pool: d.Pool, Registry: reg}).Handler()
	if err != nil {
		t.Fatal(err)
	}
	svc := environments.NewAPI(cfg, d.Pool, reg).Service
	return &fixture{t: t, db: d, handler: h, registry: reg, service: svc}
}

type response struct {
	status int
	raw    string
}

func (r response) json() map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(r.raw), &m); err != nil {
		panic(fmt.Sprintf("body %q: %v", r.raw, err))
	}
	return m
}

func (r response) list() []map[string]any {
	var l []map[string]any
	if err := json.Unmarshal([]byte(r.raw), &l); err != nil {
		panic(fmt.Sprintf("body %q: %v", r.raw, err))
	}
	return l
}

func (r response) detail() string {
	s, _ := r.json()["detail"].(string)
	return s
}

func (f *fixture) do(method, path string, body any) response {
	f.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		rd = strings.NewReader(string(b))
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, rd)
	req.Host = "api.test"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return response{rec.Code, rec.Body.String()}
}

func (f *fixture) want(r response, status int) response {
	f.t.Helper()
	if r.status != status {
		f.t.Fatalf("status = %d, want %d; body %s", r.status, status, r.raw)
	}
	return r
}

func (f *fixture) project(id, user string) {
	f.t.Helper()
	_, err := f.db.Pool.Exec(context.Background(),
		`INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ($1, $2, 'o', $1, 'o/'||$1, 'https://example.test/'||$1)`, id, user)
	if err != nil {
		f.t.Fatal(err)
	}
}

// register stores a prebuilt image through the API and returns the environment and version ids.
func (f *fixture) register(name string, opts fakeregistry.Options, extra map[string]any) (envID, versionID string) {
	f.t.Helper()
	repo := "oldsj/" + name
	img := f.registry.AddImage("ghcr.io", repo, opts)
	body := map[string]any{"name": name, "image": img.Reference()}
	for k, v := range extra {
		body[k] = v
	}
	r := f.want(f.do("POST", "/environments", body), http.StatusCreated).json()
	return r["environment"].(map[string]any)["id"].(string), r["version"].(map[string]any)["id"].(string)
}

func (f *fixture) count(table string) int {
	f.t.Helper()
	var n int
	if err := f.db.Pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestRegisterAndRead(t *testing.T) {
	f := newFixture(t)
	img := f.registry.AddImage("ghcr.io", "oldsj/dev", fakeregistry.Options{Index: true})
	r := f.want(f.do("POST", "/environments", map[string]any{"name": "dev", "image": img.Reference(), "watched_tag": "stable"}), 201).json()
	env, ver := r["environment"].(map[string]any), r["version"].(map[string]any)
	if env["owner_id"] != owner || env["name"] != "dev" || env["source_kind"] != "prebuilt_image" || env["watched_tag"] != "stable" ||
		env["accepted_default_version_id"] != nil || env["definition"] != nil {
		t.Errorf("environment = %v", env)
	}
	if ver["validation_status"] != "static_validated" || ver["architecture"] != "amd64" || ver["declared_user"] != "65532:65532" ||
		ver["index_digest"] != img.Index || ver["platform_manifest_digest"] != img.Manifest || ver["provenance_kind"] != "user_pushed" ||
		ver["validator_version"] != "oci-static-v2" || ver["environment_id"] != env["id"] {
		t.Errorf("version = %v", ver)
	}
	if vr, ok := ver["validation_result"].(map[string]any); !ok || vr["platform"] != "linux/amd64" || vr["probes"] != "not_run" {
		t.Errorf("validation_result = %v", ver["validation_result"])
	}
	if cp, ok := ver["capability_profile"].(map[string]any); !ok || len(cp) != 0 {
		t.Errorf("capability_profile = %v", ver["capability_profile"])
	}

	id := env["id"].(string)
	if got := f.want(f.do("GET", "/environments/"+id, nil), 200).json(); got["id"] != id {
		t.Errorf("get = %v", got)
	}
	if list := f.want(f.do("GET", "/environments", nil), 200).list(); len(list) != 1 || list[0]["id"] != id {
		t.Errorf("list = %v", list)
	}
	if list := f.want(f.do("GET", "/environments/"+id+"/versions", nil), 200).list(); len(list) != 1 || list[0]["id"] != ver["id"] {
		t.Errorf("versions = %v", list)
	}
	if got := f.want(f.do("GET", "/environments/"+id+"/versions/"+ver["id"].(string), nil), 200).json(); got["id"] != ver["id"] {
		t.Errorf("version = %v", got)
	}
	f.want(f.do("GET", "/environments/"+id+"/versions/nope", nil), 404)
	if r := f.want(f.do("GET", "/environments/nope", nil), 404); r.detail() != "Environment not found" {
		t.Errorf("detail = %q", r.detail())
	}
	f.want(f.do("GET", "/environments/nope/versions", nil), 404)
	f.want(f.do("GET", "/environments/nope/versions/x", nil), 404)

	// The arm64 image from the same index registers as a separate environment.
	f.want(f.do("POST", "/environments", map[string]any{"name": "arm", "image": img.Reference(), "architecture": "arm64"}), 201)
}

func TestInvalidRegistrationPersistsNothing(t *testing.T) {
	f := newFixture(t)
	root := f.registry.AddImage("ghcr.io", "oldsj/root", fakeregistry.Options{User: "0:0"})
	digest := "sha256:" + strings.Repeat("a", 64)
	for name, tc := range map[string]struct {
		body   map[string]any
		status int
		detail string
	}{
		"root user":      {map[string]any{"name": "x", "image": root.Reference()}, 422, "Declared USER must be exactly 65532:65532"},
		"not allowlist":  {map[string]any{"name": "x", "image": "docker.io/oldsj/dev@" + digest}, 422, "not allowlisted"},
		"mutable tag":    {map[string]any{"name": "x", "image": "ghcr.io/oldsj/dev:latest"}, 422, "mutable tag"},
		"missing image":  {map[string]any{"name": "x", "image": "ghcr.io/oldsj/gone@" + digest}, 422, "private images not supported yet"},
		"no image":       {map[string]any{"name": "x"}, 422, "prebuilt registration requires image only"},
		"image and def":  {map[string]any{"name": "x", "image": root.Reference(), "definition": def()}, 422, "prebuilt registration requires image only"},
		"def with image": {map[string]any{"name": "x", "source_kind": "definition_repo", "definition": def(), "image": root.Reference()}, 422, "definition registration requires definition only"},
		"def with tag":   {map[string]any{"name": "x", "source_kind": "definition_repo", "definition": def(), "watched_tag": "t"}, 422, "definition registration requires definition only"},
		"def missing":    {map[string]any{"name": "x", "source_kind": "definition_repo"}, 422, "definition registration requires definition only"},
		"def path up":    {map[string]any{"name": "x", "source_kind": "definition_repo", "definition": map[string]any{"repository": "o/r", "commit_sha": strings.Repeat("a", 40), "path": "a/../../b"}}, 422, "stay within"},
		"def path abs":   {map[string]any{"name": "x", "source_kind": "definition_repo", "definition": map[string]any{"repository": "o/r", "commit_sha": strings.Repeat("a", 40), "path": "/etc"}}, 422, "stay within"},
		"empty name":     {map[string]any{"name": "", "image": root.Reference()}, 422, ""},
		"unknown field":  {map[string]any{"name": "x", "image": root.Reference(), "owner_id": "z"}, 422, ""},
	} {
		t.Run(name, func(t *testing.T) {
			r := f.want(f.do("POST", "/environments", tc.body), tc.status)
			if tc.detail != "" && !strings.Contains(r.raw, tc.detail) {
				t.Errorf("body = %s, want %q", r.raw, tc.detail)
			}
		})
	}
	if n, v := f.count("dev_environments"), f.count("environment_versions"); n != 0 || v != 0 {
		t.Errorf("rows persisted by rejected registrations: %d environments, %d versions", n, v)
	}
}

func def() map[string]any {
	return map[string]any{"repository": "oldsj/mainloop", "commit_sha": strings.Repeat("a", 40), "path": "envs/dev"}
}

func TestDefinitionRegistrationIsPendingBuild(t *testing.T) {
	f := newFixture(t)
	r := f.want(f.do("POST", "/environments", map[string]any{"name": "built", "source_kind": "definition_repo", "definition": def()}), 201).json()
	env, ver := r["environment"].(map[string]any), r["version"].(map[string]any)
	if env["source_kind"] != "definition_repo" || env["definition"] == nil {
		t.Errorf("environment = %v", env)
	}
	if ver["validation_status"] != "pending_build" || ver["provenance_kind"] != "mainloop_built" || ver["validator_version"] != "oci-static-v2" ||
		ver["registry"] != nil || ver["platform_manifest_digest"] != nil || ver["architecture"] != nil {
		t.Errorf("version = %v", ver)
	}
	if vr := ver["validation_result"].(map[string]any); vr["build"] != "not_run" {
		t.Errorf("validation_result = %v", vr)
	}
	if n := len(f.registry.Calls()); n != 0 {
		t.Errorf("registry calls = %d, want 0", n)
	}
	envID, verID := env["id"].(string), ver["id"].(string)
	f.project("p1", owner)
	if r := f.want(f.do("PUT", "/environments/"+envID+"/default", map[string]any{"version_id": verID}), 422); r.detail() != "Version is not statically validated" {
		t.Errorf("detail = %q", r.detail())
	}
	if r := f.want(f.do("PUT", "/projects/p1/environment", map[string]any{"environment_id": envID, "version_id": verID, "expected_version": 0}), 422); r.detail() != "Version is not statically validated" {
		t.Errorf("detail = %q", r.detail())
	}
	// Refresh has no tag to watch.
	f.want(f.do("POST", "/environments/"+envID+"/refresh", map[string]any{"version_id": verID}), 422)
}

func TestSetDefault(t *testing.T) {
	f := newFixture(t)
	envID, verID := f.register("dev", fakeregistry.Options{}, nil)
	other, otherVer := f.register("other", fakeregistry.Options{}, nil)
	r := f.want(f.do("PUT", "/environments/"+envID+"/default", map[string]any{"version_id": verID}), 200).json()
	if r["accepted_default_version_id"] != verID {
		t.Errorf("env = %v", r)
	}
	f.want(f.do("PUT", "/environments/"+envID+"/default", map[string]any{"version_id": "nope"}), 404)
	f.want(f.do("PUT", "/environments/"+envID+"/default", map[string]any{"version_id": otherVer}), 404) // another environment's version
	f.want(f.do("PUT", "/environments/nope/default", map[string]any{"version_id": verID}), 404)
	if got := f.want(f.do("GET", "/environments/"+other, nil), 200).json(); got["accepted_default_version_id"] != nil {
		t.Errorf("default leaked across environments: %v", got)
	}
}

func TestRefreshAddsVersionsWithoutMovingDefault(t *testing.T) {
	f := newFixture(t)
	envID, verID := f.register("dev", fakeregistry.Options{}, map[string]any{"watched_tag": "stable"})
	f.want(f.do("PUT", "/environments/"+envID+"/default", map[string]any{"version_id": verID}), 200)

	// The tag moves to a new image (a different config digest).
	next := f.registry.AddImage("ghcr.io", "oldsj/dev", fakeregistry.Options{Index: true})
	f.registry.Put("ghcr.io", "oldsj/dev", "manifests", "stable", f.registry.Get("ghcr.io", "oldsj/dev", "manifests", next.Index))

	r := f.want(f.do("POST", "/environments/"+envID+"/refresh", map[string]any{"version_id": verID}), 201).json()
	if r["id"] == verID || r["index_digest"] != next.Index || r["validation_status"] != "static_validated" || r["environment_id"] != envID {
		t.Errorf("refreshed = %v", r)
	}
	if env := f.want(f.do("GET", "/environments/"+envID, nil), 200).json(); env["accepted_default_version_id"] != verID {
		t.Errorf("refresh moved the default: %v", env)
	}
	versions := f.want(f.do("GET", "/environments/"+envID+"/versions", nil), 200).list()
	if len(versions) != 2 || versions[0]["id"] != verID || versions[1]["id"] != r["id"] {
		t.Errorf("versions = %v", versions)
	}

	f.want(f.do("POST", "/environments/"+envID+"/refresh", map[string]any{"version_id": "nope"}), 404)
	f.want(f.do("POST", "/environments/nope/refresh", map[string]any{"version_id": verID}), 404)

	// A tag that no longer resolves leaves the version list as it was.
	f.registry.Put("ghcr.io", "oldsj/dev", "manifests", "stable", []byte(`{"schemaVersion":1}`))
	f.want(f.do("POST", "/environments/"+envID+"/refresh", map[string]any{"version_id": verID}), 422)
	if n := len(f.want(f.do("GET", "/environments/"+envID+"/versions", nil), 200).list()); n != 2 {
		t.Errorf("failed refresh left %d versions, want 2", n)
	}
}

func TestVersionsAreImmutable(t *testing.T) {
	f := newFixture(t)
	envID, verID := f.register("dev", fakeregistry.Options{}, nil)
	ctx := context.Background()
	for _, q := range []string{
		`UPDATE environment_versions SET validator_version = 'x' WHERE id = $1`,
		`UPDATE environment_versions SET validation_status = 'pending_build' WHERE id = $1`,
		`DELETE FROM environment_versions WHERE id = $1`,
	} {
		if _, err := f.db.Pool.Exec(ctx, q, verID); err == nil || !strings.Contains(err.Error(), "Environment versions are immutable") {
			t.Errorf("%s: err = %v", q, err)
		}
	}
	// A version cannot name another environment's version as its parent.
	other, _ := f.register("other", fakeregistry.Options{}, nil)
	_, err := f.db.Pool.Exec(ctx, `INSERT INTO environment_versions (id, environment_id, validation_status, validation_result, validator_version, provenance_kind, parent_version_id)
		VALUES ('child', $1, 'pending_build', '{}', 'v', 'mainloop_built', $2)`, other, verID)
	if err == nil {
		t.Error("cross-environment parent accepted")
	}
	// An environment cannot default to another environment's version.
	_, otherVer := f.register("third", fakeregistry.Options{}, nil)
	if _, err := f.db.Pool.Exec(ctx, `UPDATE dev_environments SET default_version_id = $2 WHERE id = $1`, envID, otherVer); err == nil {
		t.Error("cross-environment default accepted")
	}
	// A statically validated version needs its evidence.
	if _, err := f.db.Pool.Exec(ctx, `INSERT INTO environment_versions (id, environment_id, validation_status, validation_result, validator_version, provenance_kind)
		VALUES ('bare', $1, 'static_validated', '{}', 'v', 'user_pushed')`, envID); err == nil {
		t.Error("static_validated version without evidence accepted")
	}
}

func TestSelection(t *testing.T) {
	f := newFixture(t)
	f.project("p1", owner)
	f.project("foreign", "someone-else")
	envID, v1 := f.register("dev", fakeregistry.Options{}, map[string]any{"watched_tag": "stable"})
	otherEnv, otherVer := f.register("other", fakeregistry.Options{}, nil)
	sel := func(body map[string]any) response { return f.do("PUT", "/projects/p1/environment", body) }

	f.want(f.do("GET", "/projects/p1/environment", nil), http.StatusNoContent)
	f.want(f.do("GET", "/projects/nope/environment", nil), 404)
	f.want(f.do("GET", "/projects/foreign/environment", nil), 404)
	f.want(f.do("PUT", "/projects/foreign/environment", map[string]any{"environment_id": envID, "version_id": v1, "expected_version": 0}), 404)

	r := f.want(sel(map[string]any{"environment_id": envID, "version_id": v1, "expected_version": 0}), 200).json()
	if r["revision"] != 1.0 || r["version_id"] != v1 || r["resolved_version_id"] != v1 || r["follow_default"] != false || r["access_revoked"] != false || r["project_id"] != "p1" {
		t.Errorf("selection = %v", r)
	}
	if got := f.want(f.do("GET", "/projects/p1/environment", nil), 200).json(); got["revision"] != 1.0 || got["environment_id"] != envID {
		t.Errorf("get = %v", got)
	}

	// Stale revision.
	if r := f.want(sel(map[string]any{"environment_id": envID, "version_id": v1, "expected_version": 0}), 409); r.detail() != "Stale expected version" {
		t.Errorf("detail = %q", r.detail())
	}
	f.want(sel(map[string]any{"environment_id": envID, "version_id": v1, "expected_version": 5}), 409)

	// Target errors.
	if r := f.want(sel(map[string]any{"environment_id": envID, "follow_default": true, "expected_version": 1}), 422); r.detail() != "Environment has no accepted default" {
		t.Errorf("detail = %q", r.detail())
	}
	f.want(sel(map[string]any{"environment_id": envID, "expected_version": 1}), 422)
	f.want(sel(map[string]any{"environment_id": envID, "version_id": v1, "follow_default": true, "expected_version": 1}), 422)
	f.want(sel(map[string]any{"environment_id": envID, "follow_default": false, "expected_version": 1}), 422)
	if r := f.want(sel(map[string]any{"environment_id": envID, "version_id": otherVer, "expected_version": 1}), 404); r.detail() != "Environment version not found" {
		t.Errorf("cross-environment version: detail = %q", r.detail())
	}
	if r := f.want(sel(map[string]any{"environment_id": "nope", "version_id": v1, "expected_version": 1}), 404); r.detail() != "Environment not found" {
		t.Errorf("detail = %q", r.detail())
	}
	f.want(sel(map[string]any{"environment_id": envID, "version_id": "nope", "expected_version": 1}), 404)
	if got := f.want(f.do("GET", "/projects/p1/environment", nil), 200).json(); got["revision"] != 1.0 {
		t.Errorf("failed selections changed the revision: %v", got)
	}

	// Follow the default.
	f.want(f.do("PUT", "/environments/"+envID+"/default", map[string]any{"version_id": v1}), 200)
	r = f.want(sel(map[string]any{"environment_id": envID, "follow_default": true, "expected_version": 1}), 200).json()
	if r["revision"] != 2.0 || r["follow_default"] != true || r["version_id"] != nil || r["resolved_version_id"] != v1 {
		t.Errorf("follow selection = %v", r)
	}
	// A new default re-resolves without a new revision.
	img := f.registry.AddImage("ghcr.io", "oldsj/dev", fakeregistry.Options{Index: true})
	f.registry.Put("ghcr.io", "oldsj/dev", "manifests", "stable", f.registry.Get("ghcr.io", "oldsj/dev", "manifests", img.Index))
	v2 := f.want(f.do("POST", "/environments/"+envID+"/refresh", map[string]any{"version_id": v1}), 201).json()["id"].(string)
	f.want(f.do("PUT", "/environments/"+envID+"/default", map[string]any{"version_id": v2}), 200)
	if got := f.want(f.do("GET", "/projects/p1/environment", nil), 200).json(); got["revision"] != 2.0 || got["resolved_version_id"] != v2 {
		t.Errorf("after default change = %v", got)
	}

	// Switch environments.
	f.want(f.do("PUT", "/environments/"+otherEnv+"/default", map[string]any{"version_id": otherVer}), 200)
	r = f.want(sel(map[string]any{"environment_id": otherEnv, "version_id": otherVer, "expected_version": 2}), 200).json()
	if r["revision"] != 3.0 || r["environment_id"] != otherEnv {
		t.Errorf("switch = %v", r)
	}
}

func TestSelectionRequiresGrantForForeignEnvironment(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.project("p1", owner)
	f.project("p2", owner)
	img := f.registry.AddImage("ghcr.io", "oldsj/shared", fakeregistry.Options{})
	regd, err := f.service.Register(ctx, "other-owner", api.RegisterEnvironment{Name: "shared", Image: &[]string{img.Reference()}[0]})
	if err != nil {
		t.Fatal(err)
	}
	envID, verID := regd.Environment.Id, regd.Version.Id
	sel := func(project string, expected int) response {
		return f.do("PUT", "/projects/"+project+"/environment", map[string]any{"environment_id": envID, "version_id": verID, "expected_version": expected})
	}

	// Not mine: invisible and unmanageable through this owner's API.
	f.want(f.do("GET", "/environments/"+envID, nil), 404)
	f.want(f.do("GET", "/environments/"+envID+"/versions", nil), 404)
	if list := f.want(f.do("GET", "/environments", nil), 200).list(); len(list) != 0 {
		t.Errorf("list = %v", list)
	}
	f.want(f.do("PUT", "/environments/"+envID+"/default", map[string]any{"version_id": verID}), 404)
	f.want(f.do("PUT", "/environments/"+envID+"/grants/p1", map[string]any{"permission": "use"}), 404)
	f.want(f.do("DELETE", "/environments/"+envID+"/grants/p1", nil), 404)
	f.want(f.do("POST", "/environments/"+envID+"/refresh", map[string]any{"version_id": verID}), 404)

	if r := f.want(sel("p1", 0), 403); r.detail() != "Environment use grant required" {
		t.Errorf("detail = %q", r.detail())
	}
	// The environment's owner grants p1 only.
	if _, err := f.service.Grant(ctx, "other-owner", envID, "p1", "use"); err != nil {
		t.Fatal(err)
	}
	r := f.want(sel("p1", 0), 200).json()
	if r["access_revoked"] != false || r["resolved_version_id"] != verID {
		t.Errorf("selection = %v", r)
	}
	f.want(sel("p2", 0), 403)

	// Revoking keeps the selection but flags it, and blocks a new one.
	if err := f.service.Revoke(ctx, "other-owner", envID, "p1"); err != nil {
		t.Fatal(err)
	}
	if got := f.want(f.do("GET", "/projects/p1/environment", nil), 200).json(); got["access_revoked"] != true || got["revision"] != 1.0 {
		t.Errorf("after revoke = %v", got)
	}
	f.want(sel("p1", 1), 403)
	// A grant for a project that does not exist is a 404.
	if _, err := f.service.Grant(ctx, "other-owner", envID, "nope", "use"); err == nil || !strings.Contains(err.Error(), "Project not found") {
		t.Errorf("grant to unknown project: %v", err)
	}
	// A grant may also be a derive grant, which still allows selection.
	if _, err := f.service.Grant(ctx, "other-owner", envID, "p1", "derive"); err != nil {
		t.Fatal(err)
	}
	f.want(sel("p1", 1), 200)
}

func TestGrantAndRevokeRoutes(t *testing.T) {
	f := newFixture(t)
	f.project("p1", owner)
	f.project("foreign", "someone-else") // a grant may name any existing project
	envID, _ := f.register("dev", fakeregistry.Options{}, nil)

	r := f.want(f.do("PUT", "/environments/"+envID+"/grants/p1", map[string]any{"permission": "use"}), 200).json()
	if r["environment_id"] != envID || r["project_id"] != "p1" || r["permission"] != "use" {
		t.Errorf("grant = %v", r)
	}
	f.want(f.do("PUT", "/environments/"+envID+"/grants/p1", map[string]any{"permission": "derive"}), 200) // replaces
	var perm string
	if err := f.db.Pool.QueryRow(context.Background(), `SELECT permission FROM environment_grants WHERE environment_id = $1 AND project_id = 'p1'`, envID).Scan(&perm); err != nil || perm != "derive" {
		t.Errorf("permission = %q, %v", perm, err)
	}
	f.want(f.do("PUT", "/environments/"+envID+"/grants/foreign", map[string]any{"permission": "use"}), 200)
	if r := f.want(f.do("PUT", "/environments/"+envID+"/grants/nope", map[string]any{"permission": "use"}), 404); r.detail() != "Project not found" {
		t.Errorf("detail = %q", r.detail())
	}
	f.want(f.do("PUT", "/environments/nope/grants/p1", map[string]any{"permission": "use"}), 404)
	f.want(f.do("PUT", "/environments/"+envID+"/grants/p1", map[string]any{"permission": "admin"}), 422)
	f.want(f.do("PUT", "/environments/"+envID+"/grants/p1", map[string]any{}), 422)

	f.want(f.do("DELETE", "/environments/"+envID+"/grants/p1", nil), 204)
	f.want(f.do("DELETE", "/environments/"+envID+"/grants/p1", nil), 204) // already gone
	f.want(f.do("DELETE", "/environments/"+envID+"/grants/nope", nil), 404)
	f.want(f.do("DELETE", "/environments/nope/grants/p1", nil), 404)
	if f.count("environment_grants") != 1 {
		t.Errorf("grants = %d, want only the foreign project's", f.count("environment_grants"))
	}
}

func TestConcurrentSelectionsConflict(t *testing.T) {
	f := newFixture(t)
	f.project("p1", owner)
	envID, v1 := f.register("dev", fakeregistry.Options{}, map[string]any{"watched_tag": "stable"})
	img := f.registry.AddImage("ghcr.io", "oldsj/dev", fakeregistry.Options{Index: true})
	f.registry.Put("ghcr.io", "oldsj/dev", "manifests", "stable", f.registry.Get("ghcr.io", "oldsj/dev", "manifests", img.Index))
	v2 := f.want(f.do("POST", "/environments/"+envID+"/refresh", map[string]any{"version_id": v1}), 201).json()["id"].(string)

	for round := range 5 {
		status := make([]int, 2)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i, v := range []string{v1, v2} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				status[i] = f.do("PUT", "/projects/p1/environment", map[string]any{"environment_id": envID, "version_id": v, "expected_version": round}).status
			}()
		}
		close(start)
		wg.Wait()
		if (status[0] != 200 || status[1] != 409) && (status[0] != 409 || status[1] != 200) {
			t.Fatalf("round %d: statuses = %v, want one 200 and one 409", round, status)
		}
		if got := f.want(f.do("GET", "/projects/p1/environment", nil), 200).json(); got["revision"] != float64(round+1) {
			t.Fatalf("round %d: revision = %v", round, got["revision"])
		}
	}
}

// Grant, revoke and select touch the same project and environment rows from opposite ends.
// Whatever the interleaving, none may deadlock or fail with a server error.
func TestConcurrentGrantRevokeSelectNeverDeadlock(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f.project("p1", owner)
	img := f.registry.AddImage("ghcr.io", "oldsj/shared", fakeregistry.Options{})
	regd, err := f.service.Register(ctx, "other-owner", api.RegisterEnvironment{Name: "shared", Image: &[]string{img.Reference()}[0]})
	if err != nil {
		t.Fatal(err)
	}
	envID, verID := regd.Environment.Id, regd.Version.Id

	var wg sync.WaitGroup
	failures := make(chan string, 200)
	for worker := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 15 {
				var status int
				switch (worker + i) % 3 {
				case 0:
					_, err := f.service.Grant(ctx, "other-owner", envID, "p1", "use")
					if err != nil {
						failures <- "grant: " + err.Error()
					}
				case 1:
					if err := f.service.Revoke(ctx, "other-owner", envID, "p1"); err != nil {
						failures <- "revoke: " + err.Error()
					}
				default:
					r := f.do("GET", "/projects/p1/environment", nil)
					status = r.status
					if status != 200 && status != 204 {
						failures <- fmt.Sprintf("get: %d %s", status, r.raw)
					}
					rev := 0
					if status == 200 {
						rev = int(r.json()["revision"].(float64))
					}
					r = f.do("PUT", "/projects/p1/environment", map[string]any{"environment_id": envID, "version_id": verID, "expected_version": rev})
					if r.status != 200 && r.status != 403 && r.status != 409 {
						failures <- fmt.Sprintf("select: %d %s", r.status, r.raw)
					}
				}
			}
		}()
	}
	wg.Wait()
	close(failures)
	for msg := range failures {
		t.Error(msg)
	}
}

func TestOnlyOneProductionPathReadsRegistry(t *testing.T) {
	// The default client must not be built with credentials or a proxy; it reads only through
	// AnonymousRegistry. This guards the wiring: a nil Deps.Registry yields the anonymous client.
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test"})
	if err != nil {
		t.Fatal(err)
	}
	a := environments.NewAPI(cfg, nil, nil)
	if _, ok := a.Service.Validator.Client.(*environments.AnonymousRegistry); !ok {
		t.Errorf("default client = %T, want *AnonymousRegistry", a.Service.Validator.Client)
	}
	if got := a.Service.Validator.Allowlist; len(got) != 1 || got[0] != "ghcr.io" {
		t.Errorf("allowlist = %v", got)
	}
}
