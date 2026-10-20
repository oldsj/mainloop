package app_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/app"
	"github.com/oldsj/mainloop/server/internal/app/apicheck"
	"github.com/oldsj/mainloop/server/internal/app/apptest"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
	"github.com/oldsj/mainloop/server/internal/projects"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

// projectServer is the owner API over a scratch database and the fake GitHub.
func projectServer(t *testing.T, policyWrites bool) (http.Handler, *githubtest.Server) {
	t.Helper()
	fake := githubtest.New(t)
	desc := "the loop"
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	fake.AddRepo(githubtest.Repository{
		Owner: "oldsj", Name: "mainloop", Description: &desc, Issues: 3, AvatarURL: "https://a.example/o.png",
		PullRequests: []githubtest.PullRequest{{Number: 7, Title: "seven", Author: "o", HeadRef: "mainloop/x", CreatedAt: now, UpdatedAt: now}},
		Commits:      []githubtest.Commit{{SHA: "0123456789", Message: "msg", Author: "A", Date: now}},
	})
	gh, err := github.NewApp(github.Options{AppID: fmt.Sprint(githubtest.AppID), PrivateKey: githubtest.PrivateKey(), BaseURL: fake.URL})
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test"}
	if policyWrites {
		vars["MAINLOOP_OWNER_POLICY_WRITES_ENABLED"] = "true"
	}
	cfg, err := config.Parse(vars)
	if err != nil {
		t.Fatal(err)
	}
	pool := testdb.New(t).Pool
	var remote projects.Remote = gh
	d := apptest.Deps(t, cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)), pool, apptest.WithRemote(remote))
	d.DB = up()
	h, err := app.New(d).Handler()
	if err != nil {
		t.Fatal(err)
	}
	return apicheck.Wrap(t, h), fake
}

// strict decodes a response body into v and fails on any field the contract type lacks.
func strict(t *testing.T, body string, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
}

func importProject(t *testing.T, h http.Handler, repo string) api.Project {
	t.Helper()
	r := send(h, "POST", "/projects", fmt.Sprintf(`{"repo":%q}`, repo), nil)
	if r.status != 201 {
		t.Fatalf("import %s: %d %s", repo, r.status, r.body)
	}
	var p api.Project
	strict(t, r.body, &p)
	return p
}

func TestProjectImportListGet(t *testing.T) {
	t.Parallel()
	h, _ := projectServer(t, false)
	p := importProject(t, h, "https://github.com/OldSJ/Mainloop.git")
	if p.FullName != "oldsj/mainloop" || p.OpenPrCount != 1 || p.OpenIssueCount != 3 || p.MergePolicy != api.MergePolicyAuto ||
		p.MergePolicyVersion != 1 || p.MetadataUpdatedAt == nil || p.Id == "" {
		t.Fatalf("%+v", p)
	}
	if again := importProject(t, h, "oldsj/mainloop"); again.Id != p.Id {
		t.Fatal("second import made a second project")
	}

	r := send(h, "GET", "/projects", "", nil)
	var list []api.Project
	strict(t, r.body, &list)
	if r.status != 200 || len(list) != 1 || list[0].Id != p.Id {
		t.Fatalf("%d %s", r.status, r.body)
	}
	r = send(h, "GET", "/projects?limit=0", "", nil)
	if r.status != 200 || strings.TrimSpace(r.body) != "[]" {
		t.Fatalf("%d %s", r.status, r.body)
	}
	r = send(h, "GET", "/projects/"+p.Id, "", nil)
	var got api.Project
	strict(t, r.body, &got)
	if r.status != 200 || got.Id != p.Id {
		t.Fatalf("%d %s", r.status, r.body)
	}
}

func TestProjectErrorsAreTheContractShapes(t *testing.T) {
	t.Parallel()
	h, _ := projectServer(t, false)
	p := importProject(t, h, "oldsj/mainloop")
	for _, c := range []struct {
		name, method, path, body string
		status                   int
		detail                   string
	}{
		{"bad repo", "POST", "/projects", `{"repo":"https://gitlab.com/a/b"}`, 400, "Invalid GitHub repository"},
		{"unknown project", "GET", "/projects/nope", "", 404, "Project not found"},
		{"unknown detail", "GET", "/projects/nope/detail", "", 404, "Project not found"},
		{"unknown policy", "GET", "/projects/nope/merge-policy", "", 404, "Project not found"},
		{"policy write disabled", "PUT", "/projects/" + p.Id + "/merge-policy", `{"merge_policy":"approval","expected_version":1}`, 503, "Owner policy writes are disabled"},
	} {
		r := send(h, c.method, c.path, c.body, nil)
		if r.status != c.status || detail(t, r) != c.detail {
			t.Errorf("%s: %d %s", c.name, r.status, r.body)
		}
	}
	for name, c := range map[string]struct{ method, path, body string }{
		"import without repo":      {"POST", "/projects", `{}`},
		"import extra field":       {"POST", "/projects", `{"repo":"a/b","x":1}`},
		"import wrong type":        {"POST", "/projects", `{"repo":3}`},
		"limit not a number":       {"GET", "/projects?limit=x", ""},
		"policy unknown value":     {"PUT", "/projects/" + p.Id + "/merge-policy", `{"merge_policy":"yolo","expected_version":1}`},
		"policy version below one": {"PUT", "/projects/" + p.Id + "/merge-policy", `{"merge_policy":"auto","expected_version":0}`},
		"policy missing version":   {"PUT", "/projects/" + p.Id + "/merge-policy", `{"merge_policy":"auto"}`},
		"policy extra field":       {"PUT", "/projects/" + p.Id + "/merge-policy", `{"merge_policy":"auto","expected_version":1,"x":1}`},
	} {
		r := send(h, c.method, c.path, c.body, nil)
		if r.status != 422 {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
}

func TestProjectDetail(t *testing.T) {
	t.Parallel()
	h, _ := projectServer(t, false)
	p := importProject(t, h, "oldsj/mainloop")
	r := send(h, "GET", "/projects/"+p.Id+"/detail", "", nil)
	var d api.ProjectDetail
	strict(t, r.body, &d)
	if r.status != 200 || d.Project.Id != p.Id || len(d.OpenPrs) != 1 || d.OpenPrs[0].Number != 7 || !d.OpenPrs[0].IsMainloop ||
		len(d.RecentCommits) != 1 || d.RecentCommits[0].Sha != "0123456" || d.Sessions == nil || len(d.Sessions) != 0 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	if !strings.Contains(r.body, `"sessions":[]`) {
		t.Fatalf("sessions not an empty array: %s", r.body)
	}
}

func TestProjectDetailWhenGitHubIsDown(t *testing.T) {
	t.Parallel()
	h, fake := projectServer(t, false)
	p := importProject(t, h, "oldsj/mainloop")
	fake.Fail("GET", "/repos/oldsj/mainloop/pulls", 502)
	fake.Fail("GET", "/repos/oldsj/mainloop/commits", 502)
	r := send(h, "GET", "/projects/"+p.Id+"/detail", "", nil)
	if r.status != 200 || !strings.Contains(r.body, `"open_prs":[]`) || !strings.Contains(r.body, `"recent_commits":[]`) ||
		strings.Contains(r.body, githubtest.SecretMarker) {
		t.Fatalf("%d %s", r.status, r.body)
	}
}

func TestMergePolicyReadAndGatedCompareAndSwap(t *testing.T) {
	t.Parallel()
	h, _ := projectServer(t, true)
	p := importProject(t, h, "oldsj/mainloop")
	path := "/projects/" + p.Id + "/merge-policy"

	r := send(h, "GET", path, "", nil)
	var v api.MergePolicyView
	strict(t, r.body, &v)
	if r.status != 200 || v.MergePolicy != api.MergePolicyAuto || v.MergePolicyVersion != 1 || !v.WritesEnabled ||
		strings.Join(v.ProtectedGlobs, ",") != "k8s/**,.github/**,**/migrations/**,**/Dockerfile*" || v.ProtectedGlobsVersion != 2 {
		t.Fatalf("%d %s", r.status, r.body)
	}

	r = send(h, "PUT", path, `{"merge_policy":"approval","expected_version":1}`, nil)
	strict(t, r.body, &v)
	if r.status != 200 || v.MergePolicy != api.MergePolicyApproval || v.MergePolicyVersion != 2 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	r = send(h, "PUT", path, `{"merge_policy":"auto","expected_version":1}`, nil)
	if r.status != 409 || detail(t, r) != "Merge policy version changed" {
		t.Fatalf("stale: %d %s", r.status, r.body)
	}
	r = send(h, "GET", "/projects/"+p.Id, "", nil)
	var got api.Project
	strict(t, r.body, &got)
	if got.MergePolicy != api.MergePolicyApproval || got.MergePolicyVersion != 2 {
		t.Fatalf("project not updated: %s", r.body)
	}
	r = send(h, "PUT", "/projects/nope/merge-policy", `{"merge_policy":"auto","expected_version":1}`, nil)
	if r.status != 404 {
		t.Fatalf("%d %s", r.status, r.body)
	}
}

// Everything the server sent GitHub is on the allowlist: reads and token mints, nothing else.
func TestProjectFlowsOnlyEverCallAllowlistedEndpoints(t *testing.T) {
	t.Parallel()
	h, fake := projectServer(t, false)
	p := importProject(t, h, "oldsj/mainloop")
	send(h, "GET", "/projects/"+p.Id+"/detail", "", nil)
	for _, r := range fake.Requests() {
		ok := (r.Method == "GET" && strings.HasPrefix(r.Path, "/repos/oldsj/mainloop")) ||
			(r.Method == "POST" && strings.HasSuffix(r.Path, "/access_tokens"))
		if !ok {
			t.Errorf("unexpected GitHub call %s %s", r.Method, r.Path)
		}
	}
	if len(fake.Requests()) == 0 {
		t.Fatal("no GitHub calls recorded")
	}
}
