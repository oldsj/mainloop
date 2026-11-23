package privileged_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/privileged"
)

// The owner's audit lists every action, newest first, with the card's title once frozen, and one
// action's detail carries the card's text, the frozen facts, the result and each step, and never a
// secret value.
func TestOwnerAudit(t *testing.T) {
	e := newExecEnv(t)
	done := e.requestCopy(t, "copy-1", copyArgs(appNS, "widgets-db", "password"))
	e.runExecutor(t)
	e.run(t)
	e.approve(t, done.ID)
	e.runExecutor(t)
	waiting := e.requestCopy(t, "copy-2", copyArgs(appNS, "widgets-cache", "password"))

	a := &privileged.PrivilegedAPI{Pool: e.db.Pool, OwnerID: owner}
	ctx := context.Background()
	res, err := a.ListPrivilegedActions(ctx, api.ListPrivilegedActionsRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	list := res.(api.ListPrivilegedActions200JSONResponse)
	if len(list) != 2 || list[0].Id != waiting.ID || list[1].Id != done.ID {
		t.Fatalf("list = %+v", list)
	}
	if list[0].State != api.PrivilegedActionStateRequested || list[0].Title != privileged.TypeItemToSecret {
		t.Errorf("unfrozen = %s %q", list[0].State, list[0].Title)
	}
	if list[1].State != api.PrivilegedActionStateSucceeded || !strings.HasPrefix(list[1].Title, `Copy 1Password "widgets-db › password"`) ||
		list[1].DecidedAt == nil || list[1].FinishedAt == nil {
		t.Errorf("done = %+v", list[1])
	}

	got, err := a.GetPrivilegedAction(ctx, api.GetPrivilegedActionRequestObject{ActionId: done.ID})
	if err != nil {
		t.Fatal(err)
	}
	d := api.PrivilegedActionDetail(got.(api.GetPrivilegedAction200JSONResponse))
	if d.Body == "" || d.FrozenFacts == nil || d.Result == nil || d.Digest == nil || len(d.Events) < 3 || d.Args["namespace"] != appNS {
		t.Errorf("detail = %+v", d)
	}
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), secretValue) {
		t.Error("the detail carries the secret value")
	}

	if _, err := a.GetPrivilegedAction(ctx, api.GetPrivilegedActionRequestObject{ActionId: "nope"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing action: %v", err)
	}
	other := &privileged.PrivilegedAPI{Pool: e.db.Pool, OwnerID: "someone-else"}
	if _, err := other.GetPrivilegedAction(ctx, api.GetPrivilegedActionRequestObject{ActionId: done.ID}); err == nil {
		t.Error("another owner read the action")
	}
}
