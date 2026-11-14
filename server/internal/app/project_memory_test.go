package app_test

import (
	"context"
	"testing"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/projectcontext"
)

// TestProjectMemoryOverTheAPI: the owner lists a project's memory and accepts, rejects or revokes
// entries; a decision the entry's state does not allow is a 409, and every response matches the
// contract.
func TestProjectMemoryOverTheAPI(t *testing.T) {
	t.Parallel()
	h, _, pool := projectServerWithPool(t, false)
	p := importProject(t, h, "oldsj/mainloop")
	ctx := context.Background()
	var owner string
	if err := pool.QueryRow(ctx, `SELECT user_id FROM projects WHERE id = $1`, p.Id).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	svc := &projectcontext.Service{Pool: pool, OwnerID: owner}
	sess := "sup-1"
	main, err := svc.Remember(ctx, p.Id, projectcontext.Recorder{Author: projectcontext.AuthorMain}, "The owner reviews UI on a phone.", "")
	if err != nil {
		t.Fatal(err)
	}
	prop, err := svc.Remember(ctx, p.Id, projectcontext.Recorder{Author: projectcontext.AuthorTask, Source: projectcontext.Source{SessionID: &sess}}, "Deploys need a canary.", "")
	if err != nil {
		t.Fatal(err)
	}
	path := "/projects/" + p.Id + "/memory"

	r := send(h, "GET", path, "", nil)
	var m api.ProjectMemory
	strict(t, r.body, &m)
	if r.status != 200 || len(m.Entries) != 2 || m.Entries[0].Id != main.ID || m.Entries[1].State != api.ProjectMemoryStateProposed ||
		m.Entries[1].Source.SessionId == nil || *m.Entries[1].Source.SessionId != sess ||
		m.UsedBytes != len(main.Text) || m.MaxBytes != 4096 || m.MaxEntryBytes != 500 {
		t.Fatalf("list: %d %s", r.status, r.body)
	}

	decide := func(id, decision string) result {
		return send(h, "POST", path+"/"+id+"/decision", `{"decision":"`+decision+`"}`, nil)
	}
	var e api.ProjectMemoryEntry
	r = decide(prop.ID, "accept")
	strict(t, r.body, &e)
	if r.status != 200 || e.State != api.ProjectMemoryStateActive || e.DecidedAt == nil || e.Author != api.ProjectMemoryEntryAuthorTask {
		t.Fatalf("accept: %d %s", r.status, r.body)
	}
	if r = decide(prop.ID, "reject"); r.status != 409 {
		t.Fatalf("reject an active entry: %d %s", r.status, r.body)
	}
	r = decide(main.ID, "revoke")
	strict(t, r.body, &e)
	if r.status != 200 || e.State != api.ProjectMemoryStateForgotten {
		t.Fatalf("revoke: %d %s", r.status, r.body)
	}

	for name, c := range map[string]struct{ path, body string }{
		"missing project": {"/projects/nope/memory/" + main.ID + "/decision", `{"decision":"revoke"}`},
		"missing entry":   {path + "/nope/decision", `{"decision":"revoke"}`},
	} {
		if r := send(h, "POST", c.path, c.body, nil); r.status != 404 {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	if r := send(h, "GET", "/projects/nope/memory", "", nil); r.status != 404 || detail(t, r) != "Project not found" {
		t.Errorf("missing project list: %d %s", r.status, r.body)
	}
	for name, body := range map[string]string{
		"unknown decision": `{"decision":"maybe"}`,
		"extra field":      `{"decision":"accept","x":1}`,
		"missing":          `{}`,
	} {
		if r := send(h, "POST", path+"/"+prop.ID+"/decision", body, nil); r.status != 422 {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
}
