package app_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/api"
)

// TestProjectInstructionsOverTheAPI: the owner reads, saves and lists a project's instructions;
// a save from a stale read is a 409, an oversized or malformed one a 422, and every response
// matches the contract.
func TestProjectInstructionsOverTheAPI(t *testing.T) {
	t.Parallel()
	h, _ := projectServer(t, false) // instructions are not gated by the policy-writes switch
	p := importProject(t, h, "oldsj/mainloop")
	path := "/projects/" + p.Id + "/instructions"

	r := send(h, "GET", path, "", nil)
	var in api.ProjectInstructions
	strict(t, r.body, &in)
	if r.status != 200 || in.Version != 0 || in.Text != "" || in.UpdatedAt != nil || in.MaxBytes != 8192 || in.ProjectId != p.Id {
		t.Fatalf("unset: %d %s", r.status, r.body)
	}
	r = send(h, "PUT", path, `{"text":"UI demos use fictional data.","expected_version":0}`, nil)
	strict(t, r.body, &in)
	if r.status != 200 || in.Version != 1 || in.Text != "UI demos use fictional data." || in.UpdatedAt == nil {
		t.Fatalf("save: %d %s", r.status, r.body)
	}
	r = send(h, "PUT", path, `{"text":"something else","expected_version":0}`, nil)
	if r.status != 409 || detail(t, r) != "Project instructions changed since they were read" {
		t.Fatalf("stale: %d %s", r.status, r.body)
	}
	r = send(h, "PUT", path, fmt.Sprintf(`{"text":%q,"expected_version":1}`, strings.Repeat("é", 4097)), nil)
	if r.status != 422 || !strings.Contains(r.body, "Project instructions are longer than 8192 bytes") {
		t.Fatalf("oversized: %d %s", r.status, r.body)
	}
	r = send(h, "PUT", path, `{"text":"Merge with --squash.","expected_version":1}`, nil)
	strict(t, r.body, &in)
	if r.status != 200 || in.Version != 2 {
		t.Fatalf("second save: %d %s", r.status, r.body)
	}

	r = send(h, "GET", path+"/versions", "", nil)
	var list api.ProjectInstructionVersionList
	strict(t, r.body, &list)
	if r.status != 200 || len(list.Versions) != 2 || list.Versions[0].Version != 2 || list.Versions[1].Text != "UI demos use fictional data." {
		t.Fatalf("versions: %d %s", r.status, r.body)
	}

	for _, c := range []struct{ method, path, body string }{
		{"GET", "/projects/nope/instructions", ""},
		{"PUT", "/projects/nope/instructions", `{"text":"x","expected_version":0}`},
		{"GET", "/projects/nope/instructions/versions", ""},
	} {
		if r := send(h, c.method, c.path, c.body, nil); r.status != 404 || detail(t, r) != "Project not found" {
			t.Errorf("%s %s: %d %s", c.method, c.path, r.status, r.body)
		}
	}
	for name, body := range map[string]string{
		"missing version":  `{"text":"x"}`,
		"negative version": `{"text":"x","expected_version":-1}`,
		"extra field":      `{"text":"x","expected_version":2,"x":1}`,
		"text too long":    fmt.Sprintf(`{"text":%q,"expected_version":2}`, strings.Repeat("x", 8193)),
	} {
		if r := send(h, "PUT", path, body, nil); r.status != 422 {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
}
