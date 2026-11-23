package sessions_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
)

func (e *env) setWebFetch(sessionID string, enabled bool) (api.SessionWebFetch, error) {
	e.t.Helper()
	resp, err := e.API().SetSessionWebFetch(context.Background(), api.SetSessionWebFetchRequestObject{
		SessionId: sessionID, Body: &api.SessionWebFetchRequest{Enabled: enabled},
	})
	if err != nil {
		return api.SessionWebFetch{}, err
	}
	return api.SessionWebFetch(resp.(api.SetSessionWebFetch200JSONResponse)), nil
}

func (e *env) nativeWebFetch(sessionID string) api.SessionWebFetch {
	e.t.Helper()
	native, err := e.API().GetSessionNative(context.Background(), api.GetSessionNativeRequestObject{SessionId: sessionID})
	if err != nil {
		e.t.Fatal(err)
	}
	return api.NativeSessionInfo(native.(api.GetSessionNative200JSONResponse)).WebFetch
}

func TestASessionsWebFetchIsOffUntilTheOwnerTurnsItOn(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	// Given a new session
	id := e.newSession("start")
	// Then its agent does not have web_fetch
	if wf := e.nativeWebFetch(id); wf.Enabled || wf.FromTask || wf.SessionId != id {
		t.Fatalf("new session web_fetch = %+v", wf)
	}
	// When the owner turns it on, the session reads it back
	st, err := e.setWebFetch(id, true)
	if err != nil || !st.Enabled || st.FromTask {
		t.Fatalf("turn on = %+v, %v", st, err)
	}
	if wf := e.nativeWebFetch(id); !wf.Enabled {
		t.Fatalf("after turning on = %+v", wf)
	}
	// And when the owner turns it off again, it is off
	if st, err := e.setWebFetch(id, false); err != nil || st.Enabled {
		t.Fatalf("turn off = %+v, %v", st, err)
	}
}

func TestSetWebFetchOnAnUnknownSessionIs404(t *testing.T) {
	e := newEnv(t)
	if _, err := e.setWebFetch(uuid.NewString(), true); apierr.StatusOf(err) != 404 {
		t.Fatalf("got %v", err)
	}
}
