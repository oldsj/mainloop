package tasks_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

func TestTheOwnersTaskCarriesWebFetchOnlyWhenAsked(t *testing.T) {
	e := newEnv(t)
	a := &tasks.TasksAPI{Svc: e.svc, OwnerID: owner}
	create := func(webFetch *bool) string {
		t.Helper()
		r, err := a.CreateTask(context.Background(), api.CreateTaskRequestObject{Body: &api.TaskCreate{
			RequestId: uuid.NewString(), Title: "Research", Brief: "Read the docs", Mode: api.TaskCreateMode("coordination"), WebFetch: webFetch,
		}})
		if err != nil {
			t.Fatal(err)
		}
		return *api.TaskOperation(r.(api.CreateTask202JSONResponse)).TaskId
	}
	on := true
	// Given one task created with web_fetch and one without
	with, without := create(&on), create(nil)
	// Then only the first carries it, in the owner's view
	if v := e.view(e.own, with); v.Task.WebFetch == nil || !*v.Task.WebFetch {
		t.Errorf("task created with web_fetch: %v", v.Task.WebFetch)
	}
	if v := e.view(e.own, without); v.Task.WebFetch == nil || *v.Task.WebFetch {
		t.Errorf("task created without web_fetch: %v", v.Task.WebFetch)
	}
}
