package sessions_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/providers"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

// sentMeta is one A2A send's message id and metadata, as the gateway saw it.
type sentMeta struct {
	ID       string
	Metadata map[string]any
}

// metaLog records the metadata of every A2A send, the same way sentLog (carryover_test.go) records
// text: a reverse proxy in front of the fake kagent reads the request bodies.
type metaLog struct {
	mu   sync.Mutex
	msgs []sentMeta
}

func (l *metaLog) all() []sentMeta {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]sentMeta(nil), l.msgs...)
}

func (l *metaLog) last(t *testing.T) sentMeta {
	t.Helper()
	m := l.all()
	if len(m) == 0 {
		t.Fatal("kagent saw no message")
	}
	return m[len(m)-1]
}

// metaOpt records every send's metadata into log.
func metaOpt(t *testing.T, log *metaLog) envOpt {
	return func(so *sessions.Options, kc *kagent.Config) {
		target, err := url.Parse(kc.GatewayURL)
		if err != nil {
			t.Fatal(err)
		}
		rp := httputil.NewSingleHostReverseProxy(target)
		rp.FlushInterval = -1
		front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read", http.StatusBadRequest)
				return
			}
			var call struct {
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			var send a2a.SendMessageRequest
			if json.Unmarshal(body, &call) == nil && call.Method == "SendStreamingMessage" &&
				json.Unmarshal(call.Params, &send) == nil && send.Message != nil {
				log.mu.Lock()
				log.msgs = append(log.msgs, sentMeta{ID: send.Message.ID, Metadata: send.Message.Metadata})
				log.mu.Unlock()
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			rp.ServeHTTP(w, r)
		}))
		t.Cleanup(front.Close)
		kc.GatewayURL = front.URL
	}
}

func (e *env) setModel(sessionID string, model *string) (api.SessionModelEffort, error) {
	e.t.Helper()
	resp, err := e.API().SetSessionModel(context.Background(), api.SetSessionModelRequestObject{
		SessionId: sessionID, Body: &api.SessionModelRequest{Model: model},
	})
	if err != nil {
		return api.SessionModelEffort{}, err
	}
	return api.SessionModelEffort(resp.(api.SetSessionModel200JSONResponse)), nil
}

func (e *env) setEffort(sessionID string, effort *api.EffortLevel) (api.SessionModelEffort, error) {
	e.t.Helper()
	resp, err := e.API().SetSessionEffort(context.Background(), api.SetSessionEffortRequestObject{
		SessionId: sessionID, Body: &api.SessionEffortRequest{Effort: effort},
	})
	if err != nil {
		return api.SessionModelEffort{}, err
	}
	return api.SessionModelEffort(resp.(api.SetSessionEffort200JSONResponse)), nil
}

func strp(s string) *string             { return &s }
func effortp(e string) *api.EffortLevel { l := api.EffortLevel(e); return &l }

func TestSetSessionModelPersistsAndReturnsState(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	id := e.newSession("start")

	st, err := e.setModel(id, strp("claude-opus-5"))
	if err != nil {
		t.Fatalf("set model: %v", err)
	}
	if st.SessionId != id || st.Model == nil || *st.Model != "claude-opus-5" || st.Effort != nil {
		t.Fatalf("state %+v", st)
	}

	native, err := e.API().GetSessionNative(context.Background(), api.GetSessionNativeRequestObject{SessionId: id})
	if err != nil {
		t.Fatal(err)
	}
	info := api.NativeSessionInfo(native.(api.GetSessionNative200JSONResponse))
	if info.ModelOverride == nil || *info.ModelOverride != "claude-opus-5" {
		t.Fatalf("native model_override %v", info.ModelOverride)
	}

	// /model default clears it.
	st, err = e.setModel(id, nil)
	if err != nil {
		t.Fatalf("clear model: %v", err)
	}
	if st.Model != nil {
		t.Fatalf("state after clear %+v", st)
	}
}

func TestSetSessionModelUnknownSessionIs404(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	if _, err := e.setModel(uuid.NewString(), strp("claude-opus-5")); apierr.StatusOf(err) != 404 {
		t.Fatalf("got %v", err)
	}
}

func TestSetSessionEffortRejectsLevelNotOfferedByProvider(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	id := e.newSession("start") // defaults to the Claude profile

	// "minimal" is a Codex level, not one of Claude's.
	if _, err := e.setEffort(id, effortp("minimal")); apierr.StatusOf(err) != 422 {
		t.Fatalf("got %v", err)
	}
}

func TestSetSessionEffortAcceptsAProviderLevel(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	id := e.newSession("start")

	st, err := e.setEffort(id, effortp("xhigh"))
	if err != nil {
		t.Fatalf("set effort: %v", err)
	}
	if st.Effort == nil || *st.Effort != "xhigh" {
		t.Fatalf("state %+v", st)
	}
}

// withProductionShapedClaudeOverride gives the service a registry built the way production's
// PROVIDER_PROFILES does: an operator profile for id "claude" that only overrides agents and
// capabilities, naming no models or effort levels of its own.
func (e *env) withProductionShapedClaudeOverride() {
	e.t.Helper()
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "PROVIDER_PROFILES": `[{
		"id": "claude",
		"display_name": "Claude Co",
		"native_provider": "claude",
		"configuration_revision": "prod-v1",
		"agents": {
			"main": {"namespace": "kagent", "name": "mainloop-main"},
			"supervisor": {"namespace": "kagent", "name": "claude-workspace"},
			"child": {"namespace": "kagent", "name": "claude-subscription"},
			"agent": {"namespace": "kagent", "name": "claude-workspace"}
		},
		"capabilities": [{"capability": "native_create", "state": "proved", "scope": "live", "evidence_ref": "smoke-1"}]
	}]`})
	if err != nil {
		e.t.Fatal(err)
	}
	reg, err := providers.NewRegistry(cfg.Kagent)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, p := range reg.Profiles() {
		for _, a := range p.Agents {
			e.srv.AddAgent(a.Namespace, a.Name)
		}
	}
	e.svc.Providers = reg
}

func TestSetSessionEffortAcceptsABuiltinLevelWhenOperatorProfileOmitsEffort(t *testing.T) {
	e := newEnv(t)
	e.withProductionShapedClaudeOverride()

	claude, ok := e.svc.Providers.Profile("claude")
	if !ok || len(claude.Models) == 0 || claude.DefaultModel == "" || len(claude.EffortLevels) == 0 || claude.DefaultEffort == "" {
		t.Fatalf("operator claude profile did not inherit built-in models/effort: %+v", claude)
	}

	id := e.newSession("start") // defaults to the Claude profile
	st, err := e.setEffort(id, effortp("high"))
	if err != nil {
		t.Fatalf("set effort: %v", err)
	}
	if st.Effort == nil || *st.Effort != "high" {
		t.Fatalf("state %+v", st)
	}
}

func TestClaudeTurnMetadataOmitsUntouchedKeys(t *testing.T) {
	var log metaLog
	e := newEnv(t, metaOpt(t, &log))
	e.withProviders()
	id := e.newSession("start")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)

	sent := log.last(t)
	if len(sent.Metadata) != 0 {
		t.Fatalf("untouched Claude metadata = %+v, want none", sent.Metadata)
	}
}

func TestClaudeTurnMetadataNamesOnlyTheOverriddenKey(t *testing.T) {
	var log metaLog
	e := newEnv(t, metaOpt(t, &log))
	e.withProviders()
	id := e.newSession("start")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)

	if _, err := e.setModel(id, strp("claude-opus-5")); err != nil {
		t.Fatalf("set model: %v", err)
	}
	mid := uuid.NewString()
	if _, err := e.svc.SendTo(context.Background(), id, mid, "again"); err != nil {
		t.Fatalf("send: %v", err)
	}
	e.waitState(mid, dbstate.DeliveryCompleted)

	sent := log.last(t)
	if len(sent.Metadata) != 1 || sent.Metadata[kagent.ModelMetadataKey] != "claude-opus-5" {
		t.Fatalf("metadata = %+v", sent.Metadata)
	}
}

func TestCodexOnceTouchedNamesBothKeysExplicitlyEvenOnDefault(t *testing.T) {
	var log metaLog
	e := newEnv(t, metaOpt(t, &log))
	e.withProviders()
	id, err := e.svc.Create(context.Background(), sessions.NewSession{
		Title: "Fix the thing", Description: "d", Prompt: "start", AgentKind: "codex",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)

	profile, ok := e.svc.Providers.Profile("codex")
	if !ok {
		t.Fatal("no codex profile")
	}

	// Touch the session with a non-default effort.
	if _, err := e.setEffort(id, effortp("high")); err != nil {
		t.Fatalf("set effort: %v", err)
	}
	firstTurn := uuid.NewString()
	if _, err := e.svc.SendTo(context.Background(), id, firstTurn, "again"); err != nil {
		t.Fatalf("send: %v", err)
	}
	e.waitState(firstTurn, dbstate.DeliveryCompleted)
	sent := log.last(t)
	if sent.Metadata[kagent.ModelMetadataKey] != profile.DefaultModel || sent.Metadata[kagent.EffortMetadataKey] != "high" {
		t.Fatalf("first touched turn metadata = %+v", sent.Metadata)
	}

	// Reset back to default: the real default values are still sent explicitly, not omitted.
	if _, err := e.setEffort(id, nil); err != nil {
		t.Fatalf("reset effort: %v", err)
	}
	secondTurn := uuid.NewString()
	if _, err := e.svc.SendTo(context.Background(), id, secondTurn, "once more"); err != nil {
		t.Fatalf("send: %v", err)
	}
	e.waitState(secondTurn, dbstate.DeliveryCompleted)
	sent = log.last(t)
	if sent.Metadata[kagent.ModelMetadataKey] != profile.DefaultModel || sent.Metadata[kagent.EffortMetadataKey] != profile.DefaultEffort {
		t.Fatalf("reset turn metadata = %+v, want explicit defaults %q/%q",
			sent.Metadata, profile.DefaultModel, profile.DefaultEffort)
	}
}
