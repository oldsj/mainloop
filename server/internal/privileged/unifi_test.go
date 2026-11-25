package privileged_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/privileged"
)

// unifiKeyFixture stands for the console's API key the executor holds: no card, fact or result may
// carry it.
const unifiKeyFixture = "fictional-unifi-key-0123456789abcdef"

const networkPath = "/proxy/network/api/s/default/rest/networkconf/65a1b2c3d4e5f60718293a4b"

// guestNetwork is the console's answer for networkPath: its passphrase must never show.
const guestNetwork = `{"data":[{"_id":"65a1b2c3d4e5f60718293a4b","enabled":true,"name":"guest","vlan":60,` +
	`"x_passphrase":"fictional-wifi-passphrase"}],"meta":{"rc":"ok"}}`

var consoleHello = privileged.UniFiHello{Address: "192.0.2.2:443", Certificate: "SHA256:ZmljdGlvbmFsLWNvbnNvbGUtY2VydGlmaWNhdGU"}

// fakeUniFi is the console. objects answer GETs by path (404 otherwise), unless passGets sends
// GETs of other paths to handle; handle answers the rest.
type fakeUniFi struct {
	hello    privileged.UniFiHello
	helloErr error
	objects  map[string]string
	passGets bool
	reqs     []privileged.UniFiRequest
	handle   func(req privileged.UniFiRequest) (privileged.UniFiResponse, error)
}

func (f *fakeUniFi) Hello(context.Context) (privileged.UniFiHello, error) { return f.hello, f.helloErr }

func (f *fakeUniFi) Do(_ context.Context, req privileged.UniFiRequest) (privileged.UniFiResponse, error) {
	f.reqs = append(f.reqs, req)
	if _, known := f.objects[req.Path]; req.Method == "GET" && (known || !f.passGets) {
		body, ok := f.objects[req.Path]
		if !ok {
			return privileged.UniFiResponse{Status: 404, Body: []byte(`{"meta":{"rc":"error","msg":"api.err.NotFound"}}`), APIKey: unifiKeyFixture}, nil
		}
		return privileged.UniFiResponse{Status: 200, Body: []byte(body), APIKey: unifiKeyFixture}, nil
	}
	res, err := f.handle(req)
	res.APIKey = unifiKeyFixture
	return res, err
}

// sent are the requests other than GETs.
func (f *fakeUniFi) sent() []privileged.UniFiRequest {
	var out []privileged.UniFiRequest
	for _, r := range f.reqs {
		if r.Method != "GET" {
			out = append(out, r)
		}
	}
	return out
}

func answered(status int, body string) func(privileged.UniFiRequest) (privileged.UniFiResponse, error) {
	return func(privileged.UniFiRequest) (privileged.UniFiResponse, error) {
		return privileged.UniFiResponse{Status: status, Body: []byte(body)}, nil
	}
}

func newUniFiEnv(t *testing.T, handle func(privileged.UniFiRequest) (privileged.UniFiResponse, error)) (*kubeEnv, *fakeUniFi) {
	t.Helper()
	e := newKubeEnv(t)
	e.svc.UniFiOn = true
	f := &fakeUniFi{hello: consoleHello, objects: map[string]string{networkPath: guestNetwork}, handle: handle}
	e.ex.UniFi = f
	return e, f
}

func unifiArgs(method, path string, body any) map[string]any {
	a := map[string]any{"method": method, "path": path}
	if body != nil {
		a["body"] = body
	}
	return a
}

func unifiResult(t *testing.T, a privileged.Action) privileged.UniFiResult {
	t.Helper()
	var res privileged.UniFiResult
	if err := json.Unmarshal(a.Result, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

// Given a network on the console, when main requests a PUT to it, then preflight checks the
// console's pinned certificate and reads the network with a GET of the same path, and the card
// shows the request, its body and the network as it is now, without its passphrase. The agent's
// facts have neither the address nor the network. After an Approve the executor reads it again,
// sends the PUT once and returns the console's answer without the key or the passphrase.
func TestUniFiPutPreviewsAndRunsOnce(t *testing.T) {
	e, f := newUniFiEnv(t, answered(200, `{"data":[{"_id":"65a1b2c3d4e5f60718293a4b","enabled":false,"x_passphrase":"fictional-wifi-passphrase"}],`+
		`"meta":{"rc":"ok"},"echo":"`+unifiKeyFixture+`"}`))
	a := e.request(t, privileged.TypeUniFi, "u-1", unifiArgs("put", networkPath, map[string]any{"name": "guest", "enabled": false}))
	a, card := e.preflighted(t, a.ID)
	cardHas(t, card, "Send PUT to the UniFi console", "UniFi console (192.0.2.2:443)", "PUT "+networkPath,
		"With this body", `"enabled": false`, "What is there now", `"x_passphrase": "[redacted]"`, `"vlan": 60`,
		consoleHello.Certificate, "sends nothing if it changed", "goes back to the agent and is stored with the action",
		"Risk: UniFi API, PUT, changes the network, which can cut access to it, Mainloop's included.")
	if strings.Contains(card, "fictional-wifi-passphrase") || strings.Contains(card, "no preview") {
		t.Fatalf("card:\n%s", card)
	}
	if seen := string(privileged.AgentFacts(a.Type, a.Facts)); strings.Contains(seen, "192.0.2.2") || strings.Contains(seen, "vlan") ||
		!strings.Contains(seen, "owner's card only") || !strings.Contains(seen, consoleHello.Certificate) {
		t.Fatalf("the agent sees %s", seen)
	}
	e.approve(t, a.ID)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded {
		t.Fatalf("after execution: %+v (%v)", a, deref(a.LastError))
	}
	sent := f.sent()
	if len(sent) != 1 || sent[0].Method != "PUT" || sent[0].Path != networkPath || string(sent[0].Body) != `{"enabled":false,"name":"guest"}` ||
		sent[0].Expect == nil || *sent[0].Expect != consoleHello || len(f.reqs) != 3 {
		t.Fatalf("requests = %+v", f.reqs)
	}
	res := unifiResult(t, a)
	if res.Status != 200 || !strings.Contains(res.Body, `"enabled": false`) || strings.Contains(res.Body, unifiKeyFixture) ||
		strings.Contains(res.Body, "fictional-wifi-passphrase") {
		t.Fatalf("result = %+v", res)
	}
}

// Between the card and the request, a changed object, certificate or address, or a console that
// refuses the connection, is stale, and nothing is sent.
func TestUniFiStaleBeforeTheRequest(t *testing.T) {
	for name, tc := range map[string]func(f *fakeUniFi){
		"object":      func(f *fakeUniFi) { f.objects[networkPath] = strings.Replace(guestNetwork, `"vlan":60`, `"vlan":61`, 1) },
		"gone":        func(f *fakeUniFi) { delete(f.objects, networkPath) },
		"certificate": func(f *fakeUniFi) { f.hello.Certificate = "SHA256:b3RoZXItY2VydGlmaWNhdGU" },
		"refused":     func(f *fakeUniFi) { f.helloErr = privileged.Refusal("the UniFi console presented a certificate that is not the pinned one") },
	} {
		t.Run(name, func(t *testing.T) {
			e, f := newUniFiEnv(t, answered(200, `{}`))
			a := e.request(t, privileged.TypeUniFi, "u-1", unifiArgs("PATCH", networkPath, map[string]any{"vlan": 61}))
			a, _ = e.preflighted(t, a.ID)
			e.approve(t, a.ID)
			tc(f)
			e.runExecutor(t)
			if a = e.get(t, a.ID); a.State != privileged.StateStale || !strings.Contains(deref(a.LastError), "nothing was sent") || len(f.sent()) != 0 {
				t.Fatalf("action = %+v (%v), sent %+v", a, deref(a.LastError), f.sent())
			}
		})
	}

	// The comparison is of what the card showed: a change only to a redacted field is not seen.
	e, f := newUniFiEnv(t, answered(200, `{}`))
	a := e.request(t, privileged.TypeUniFi, "u-2", unifiArgs("PATCH", networkPath, map[string]any{"vlan": 61}))
	a, _ = e.preflighted(t, a.ID)
	e.approve(t, a.ID)
	f.objects[networkPath] = strings.Replace(guestNetwork, "fictional-wifi", "other-wifi", 1)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded || len(f.sent()) != 1 {
		t.Fatalf("action = %+v (%v)", a, deref(a.LastError))
	}
}

// A POST has no preview and says so; a GET only reads; a DELETE, or a command that forgets, is
// tagged destructive and expires in an hour; a DELETE of nothing is refused at preflight.
func TestUniFiPreviewsAndTags(t *testing.T) {
	t.Run("post", func(t *testing.T) {
		e, f := newUniFiEnv(t, answered(200, `{}`))
		a := e.request(t, privileged.TypeUniFi, "p-1", unifiArgs("POST", "/proxy/network/api/s/default/cmd/devmgr",
			map[string]any{"cmd": "restart", "mac": "02:00:5e:00:53:01"}))
		_, card := e.preflighted(t, a.ID)
		cardHas(t, card, "Nothing can preview it.", "may restart or reprovision devices (restart)", "no preview")
		if got := a.ExpiresAt.Sub(a.CreatedAt).Round(time.Minute); got != privileged.UndecidedExpiry || len(f.reqs) != 0 {
			t.Fatalf("expires after %v; a POST's preflight sent %+v", got, f.reqs)
		}
	})
	t.Run("get", func(t *testing.T) {
		e, f := newUniFiEnv(t, answered(200, `{}`))
		a := e.request(t, privileged.TypeUniFi, "g-1", unifiArgs("GET", "/proxy/network/integration/v1/sites?limit=5", nil))
		_, card := e.preflighted(t, a.ID)
		cardHas(t, card, "It only reads.", "Risk: UniFi API, GET.")
		if strings.Contains(card, "changes the network") || len(f.reqs) != 0 {
			t.Fatalf("card:\n%s\nrequests %+v", card, f.reqs)
		}
	})
	t.Run("delete", func(t *testing.T) {
		e, _ := newUniFiEnv(t, answered(200, `{}`))
		a := e.request(t, privileged.TypeUniFi, "d-1", unifiArgs("DELETE", networkPath, nil))
		if got := a.ExpiresAt.Sub(a.CreatedAt).Round(time.Minute); got != privileged.DeleteExpiry {
			t.Fatalf("a DELETE expires after %v", got)
		}
		_, card := e.preflighted(t, a.ID)
		cardHas(t, card, "Risk: destructive: may delete, forget or reset (DELETE), UniFi API, DELETE")
	})
	t.Run("forget", func(t *testing.T) {
		e, _ := newUniFiEnv(t, answered(200, `{}`))
		a := e.request(t, privileged.TypeUniFi, "f-1", unifiArgs("POST", "/proxy/network/api/s/default/cmd/stamgr",
			map[string]any{"cmd": "forget-sta", "macs": []any{"02:00:5e:00:53:02"}}))
		if got := a.ExpiresAt.Sub(a.CreatedAt).Round(time.Minute); got != privileged.DeleteExpiry {
			t.Fatalf("a forget expires after %v", got)
		}
		_, card := e.preflighted(t, a.ID)
		cardHas(t, card, "Risk: destructive: may delete, forget or reset (forget)")
	})
	t.Run("delete of nothing", func(t *testing.T) {
		e, _ := newUniFiEnv(t, answered(200, `{}`))
		a := e.request(t, privileged.TypeUniFi, "d-2", unifiArgs("DELETE", networkPath+"0", nil))
		e.runExecutor(t)
		e.run(t)
		if a = e.get(t, a.ID); a.State != privileged.StatePreflightFailed || !strings.Contains(deref(a.LastError), "nothing at that path") {
			t.Fatalf("action = %+v (%v)", a, deref(a.LastError))
		}
	})
}

// A request that cannot have reached the console fails; one whose answer was lost is uncertain
// (never sent again), but a GET whose answer was lost only failed; a 4xx fails with the answer.
func TestUniFiOutcomes(t *testing.T) {
	for name, tc := range map[string]struct {
		method string
		handle func(privileged.UniFiRequest) (privileged.UniFiResponse, error)
		state  string
		why    string
	}{
		"not sent": {"POST", func(privileged.UniFiRequest) (privileged.UniFiResponse, error) {
			return privileged.UniFiResponse{}, &privileged.NothingRan{Err: errors.New("the connection broke before the request was sent")}
		}, privileged.StateFailed, "nothing was sent to the UniFi console"},
		"lost": {"POST", func(privileged.UniFiRequest) (privileged.UniFiResponse, error) {
			return privileged.UniFiResponse{}, errors.New("the connection broke before the console answered")
		}, privileged.StateUncertain, "the console may have applied it"},
		"refused": {"POST", answered(400, `{"meta":{"rc":"error","msg":"api.err.InvalidPayload"}}`), privileged.StateFailed, "answered 400"},
	} {
		t.Run(name, func(t *testing.T) {
			e, f := newUniFiEnv(t, tc.handle)
			a := e.request(t, privileged.TypeUniFi, "u-1", unifiArgs(tc.method, "/proxy/network/api/s/default/rest/portforward",
				map[string]any{"name": "web", "dst_port": "8443"}))
			a, _ = e.preflighted(t, a.ID)
			e.approve(t, a.ID)
			e.runExecutor(t)
			e.runExecutor(t)
			if a = e.get(t, a.ID); a.State != tc.state || !strings.Contains(deref(a.LastError), tc.why) || len(f.sent()) != 1 {
				t.Fatalf("action = %+v (%v), sent %d", a, deref(a.LastError), len(f.sent()))
			}
		})
	}
	e, f := newUniFiEnv(t, nil)
	f.passGets = true
	f.handle = func(privileged.UniFiRequest) (privileged.UniFiResponse, error) {
		return privileged.UniFiResponse{}, errors.New("the console's answer was cut off")
	}
	a := e.request(t, privileged.TypeUniFi, "g-1", unifiArgs("GET", "/proxy/network/api/s/default/stat/health", nil))
	a, _ = e.preflighted(t, a.ID)
	e.approve(t, a.ID)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateFailed || !strings.Contains(deref(a.LastError), "a GET changes nothing") {
		t.Fatalf("action = %+v (%v)", a, deref(a.LastError))
	}
}

// Requests the executor never sends are refused before anything connects.
func TestUniFiRequestRefusals(t *testing.T) {
	e, f := newUniFiEnv(t, answered(200, `{}`))
	for i, tc := range []struct {
		args map[string]any
		want string
	}{
		{unifiArgs("OPTIONS", networkPath, nil), "method is one of"},
		{unifiArgs("GET", "/api/users/self", nil), "Network application path"},
		{unifiArgs("GET", "/proxy/protect/api/cameras", nil), "Network application path"},
		{unifiArgs("GET", "/proxy/network/api/s/default/../../../api/system", nil), "a . or .. segment"},
		{unifiArgs("GET", "/proxy/network/api/s/default/rest/%2e%2e", nil), "Network application path"},
		{unifiArgs("GET", "/proxy/network/api//s/default", nil), "Network application path"},
		{unifiArgs("GET", "/proxy/network/api/s/default/stat/health?a=b#x", nil), "printable ASCII"},
		{unifiArgs("GET", "/proxy/network/api/s/default/stat/health?a=b c", nil), "printable ASCII"},
		{unifiArgs("GET", "/proxy/network/api/stat/admin", nil), "the owner's own"},
		{unifiArgs("POST", "/proxy/network/api/s/default/cmd/sitemgr", map[string]any{"cmd": "invite-admin"}), "the owner's own"},
		{unifiArgs("POST", "/proxy/network/api/s/default/cmd/backup", map[string]any{"cmd": "backup"}), "the owner's own"},
		{unifiArgs("POST", "/proxy/network/integration/v1/apikeys", map[string]any{"name": "x"}), "the owner's own"},
		{unifiArgs("GET", networkPath, map[string]any{"a": 1}), "a GET has no body"},
		{unifiArgs("DELETE", networkPath, map[string]any{"a": 1}), "a DELETE has no body"},
		{unifiArgs("PUT", networkPath, "guest"), "a JSON object or array"},
		{unifiArgs("PUT", networkPath, map[string]any{"x_passphrase": "fictional-wifi-passphrase"}), "sets x_passphrase, a secret field"},
		{unifiArgs("PUT", networkPath, map[string]any{"wifi": map[string]any{"securityConfiguration": map[string]any{"passphrase": "p"}}}),
			"sets passphrase, a secret field"},
		{unifiArgs("POST", "/proxy/network/api/s/default/rest/radiusprofile", map[string]any{"auth_servers": []any{map[string]any{"x_secret": "s"}}}),
			"sets x_secret"},
		{unifiArgs("PUT", networkPath, map[string]any{"note": "Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123456789"}), "contains_secret"},
		{unifiArgs("PUT", networkPath, map[string]any{"name": strings.Repeat("x", privileged.MaxUniFiBodyBytes)}), "at most 64 KiB"},
		{map[string]any{"method": "GET", "path": networkPath, "target": "unifi"}, "unknown field"},
	} {
		_, err := e.svc.Request(context.Background(), mainSession, privileged.TypeUniFi, jsonArgs(tc.args), "r", fmt.Sprintf("r%d", i))
		if !errors.Is(err, privileged.ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.args, err, tc.want)
		}
	}
	if len(f.reqs) != 0 {
		t.Fatalf("requests %+v", f.reqs)
	}
	// An empty secret field is no secret: clearing one is allowed.
	if _, err := e.svc.Request(context.Background(), mainSession, privileged.TypeUniFi,
		jsonArgs(unifiArgs("PUT", networkPath, map[string]any{"x_passphrase": "", "name": "guest"})), "r", "ok"); err != nil {
		t.Fatalf("an empty secret field: %v", err)
	}
}

// Without UniFi access, serve-api answers http.unifi unavailable; without a client, the executor
// refuses it at preflight.
func TestUniFiNeedsAccess(t *testing.T) {
	e, _ := newUniFiEnv(t, answered(200, `{}`))
	e.svc.UniFiOn = false
	if _, err := e.svc.Request(context.Background(), mainSession, privileged.TypeUniFi, jsonArgs(unifiArgs("GET", networkPath, nil)), "r", "r1"); !errors.Is(err, privileged.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
	e.svc.UniFiOn = true
	e.ex.UniFi = nil
	a := e.request(t, privileged.TypeUniFi, "u-1", unifiArgs("GET", networkPath, nil))
	e.runExecutor(t)
	e.run(t)
	if a = e.get(t, a.ID); a.State != privileged.StatePreflightFailed || !strings.Contains(deref(a.LastError), "no UniFi access") {
		t.Fatalf("action = %+v (%v)", a, deref(a.LastError))
	}
}
