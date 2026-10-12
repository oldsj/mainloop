package smoke

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// jsonHandler answers 200 with a fixed JSON body.
func jsonHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

func statusHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// silentEvents opens the stream and never sends a frame, so the events check runs out of its wait.
func silentEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
	<-r.Context().Done()
}

// guardMethods fails the test on any method but the two the read-only checks use: GET, and the POST to
// /mcp that the MCP check makes. A write from a read-only run shows up here.
func guardMethods(t *testing.T, next http.Handler) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || (r.Method == http.MethodPost && r.URL.Path == "/mcp") {
			next.ServeHTTP(w, r)
			return
		}
		t.Errorf("read-only check sent %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
}

// stubAPI serves the owner API with every read healthy. overrides replace single routes, so a case
// changes one answer and keeps the rest.
func stubAPI(t *testing.T, overrides map[string]http.HandlerFunc) string {
	t.Helper()
	routes := map[string]http.HandlerFunc{
		"/health":      jsonHandler(`{"status":"healthy","database":"ok"}`),
		"/projects":    jsonHandler(`[]`),
		"/sessions":    jsonHandler(`[]`),
		"/workspaces":  jsonHandler(`[]`),
		"/attention":   jsonHandler(`{"count":0,"items":[],"sessions":[]}`),
		"/main-thread": jsonHandler(`{"mode":"native"}`),
		"/events": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: connected\ndata: {}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		},
	}
	for path, h := range overrides {
		routes[path] = h
	}
	mux := http.NewServeMux()
	for path, h := range routes {
		mux.HandleFunc(path, h)
	}
	srv := httptest.NewServer(guardMethods(t, mux))
	t.Cleanup(srv.Close)
	return srv.URL
}

// statusServer answers every request with one status, for the listeners whose answer is the status alone.
func statusServer(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(guardMethods(t, statusHandler(status, body)))
	t.Cleanup(srv.Close)
	return srv.URL
}

// gitStub answers a git discovery request. A request for the other service is a 400, so a probe that
// asks for the wrong service fails its check instead of passing by accident.
func gitStub(t *testing.T, service string, status int, body string) string {
	t.Helper()
	answer := statusHandler(status, body)
	srv := httptest.NewServer(guardMethods(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("service") != service {
			statusHandler(http.StatusBadRequest, "wrong service")(w, r)
			return
		}
		answer(w, r)
	})))
	t.Cleanup(srv.Close)
	return srv.URL
}

// healthyOptions is a set of options where every read-only check passes. A case changes one field to
// make one check fail or skip.
func healthyOptions(t *testing.T) ReadOptions {
	t.Helper()
	return ReadOptions{
		Endpoints: Endpoints{
			API:     stubAPI(t, nil),
			MCP:     statusServer(t, http.StatusUnauthorized, ""),
			GitRead: gitStub(t, "git-upload-pack", http.StatusUnauthorized, ""),
			GitPush: gitStub(t, "git-receive-pack", http.StatusUnauthorized, "authentication_required req-1"),
		},
		Timeout: 5 * time.Second,
	}
}

func runReadOnly(opts ReadOptions) []Check {
	var checks []Check
	ReadOnly(context.Background(), opts, func(c Check) { checks = append(checks, c) })
	return checks
}

func findCheck(t *testing.T, checks []Check, name string) Check {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in %v", name, checks)
	return Check{}
}

func TestReadOnlyAllHealthy(t *testing.T) {
	checks := runReadOnly(healthyOptions(t))
	want := []struct {
		name   string
		status Status
	}{
		{"health", OK}, {"projects", OK}, {"sessions", OK}, {"workspaces", OK}, {"attention", OK},
		{"main-thread", OK}, {"events", OK}, {"mcp", OK}, {"git-read", OK}, {"git-push", OK},
		{"kagent", Skip},
	}
	if len(checks) != len(want) {
		t.Fatalf("got %d checks, want %d: %v", len(checks), len(want), checks)
	}
	for i, w := range want {
		if checks[i].Name != w.name || checks[i].Status != w.status {
			t.Errorf("check %d = %s, want %s %s", i, checks[i], w.status, w.name)
		}
	}
}

func TestReadOnlyChecks(t *testing.T) {
	tests := []struct {
		name   string
		check  string
		mutate func(t *testing.T, o *ReadOptions)
		want   Status
		detail string // substring the detail must contain
	}{
		{"health reports unhealthy", "health", func(t *testing.T, o *ReadOptions) {
			o.API = stubAPI(t, map[string]http.HandlerFunc{"/health": jsonHandler(`{"status":"unhealthy","database":"down"}`)})
		}, Fail, "status=unhealthy"},
		{"health answers 500", "health", func(t *testing.T, o *ReadOptions) {
			o.API = stubAPI(t, map[string]http.HandlerFunc{"/health": statusHandler(500, "")})
		}, Fail, "status 500"},
		{"projects answers 500", "projects", func(t *testing.T, o *ReadOptions) {
			o.API = stubAPI(t, map[string]http.HandlerFunc{"/projects": statusHandler(500, "")})
		}, Fail, "status 500"},
		{"sessions body does not decode", "sessions", func(t *testing.T, o *ReadOptions) {
			o.API = stubAPI(t, map[string]http.HandlerFunc{"/sessions": jsonHandler(`{"not":"a list"}`)})
		}, Fail, "decode"},
		{"main-thread reports its mode", "main-thread", func(t *testing.T, o *ReadOptions) {}, OK, "mode=native"},
		{"events stream ends with no connected frame", "events", func(t *testing.T, o *ReadOptions) {
			o.API = stubAPI(t, map[string]http.HandlerFunc{"/events": func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "event: other\n\n")
			}})
		}, Fail, "stream ended before a connected frame"},
		{"events answers 503", "events", func(t *testing.T, o *ReadOptions) {
			o.API = stubAPI(t, map[string]http.HandlerFunc{"/events": statusHandler(503, "")})
		}, Fail, "status 503"},
		{"events wait runs out with no connected frame", "events", func(t *testing.T, o *ReadOptions) {
			o.API = stubAPI(t, map[string]http.HandlerFunc{"/events": silentEvents})
			o.eventsWait = 100 * time.Millisecond
		}, Fail, "no connected frame within 100ms"},
		{"mcp accepts a request with no bearer", "mcp", func(t *testing.T, o *ReadOptions) {
			o.MCP = statusServer(t, http.StatusOK, "")
		}, Fail, "status 200 without a bearer"},
		{"git read is off", "git-read", func(t *testing.T, o *ReadOptions) {
			o.GitRead = gitStub(t, "git-upload-pack", http.StatusNotImplemented, "")
		}, Skip, "transport off"},
		{"git read serves without a capability", "git-read", func(t *testing.T, o *ReadOptions) {
			o.GitRead = gitStub(t, "git-upload-pack", http.StatusOK, "")
		}, Fail, "status 200 without a capability"},
		{"git read refuses with 403", "git-read", func(t *testing.T, o *ReadOptions) {
			o.GitRead = gitStub(t, "git-upload-pack", http.StatusForbidden, "")
		}, OK, "403 without a capability"},
		{"git push is off", "git-push", func(t *testing.T, o *ReadOptions) {
			o.GitPush = gitStub(t, "git-receive-pack", http.StatusForbidden, "git_transport_disabled req-1")
		}, Skip, "transport off"},
		{"git push refuses a missing capability with 401", "git-push", func(t *testing.T, o *ReadOptions) {
			o.GitPush = gitStub(t, "git-receive-pack", http.StatusUnauthorized, "authentication_required req-1")
		}, OK, "401 authentication_required"},
		{"git push refuses with a bare 403", "git-push", func(t *testing.T, o *ReadOptions) {
			o.GitPush = gitStub(t, "git-receive-pack", http.StatusForbidden, "forbidden")
		}, Fail, "status 403"},
		{"git push refused by the host check", "git-push", func(t *testing.T, o *ReadOptions) {
			o.GitPush = gitStub(t, "git-receive-pack", http.StatusForbidden, "host_denied req-1")
		}, Fail, "host_denied"},
		{"git push refused by the path check", "git-push", func(t *testing.T, o *ReadOptions) {
			o.GitPush = gitStub(t, "git-receive-pack", http.StatusForbidden, "path_invalid req-1")
		}, Fail, "path_invalid"},
		{"git push refused by the route check", "git-push", func(t *testing.T, o *ReadOptions) {
			o.GitPush = gitStub(t, "git-receive-pack", http.StatusForbidden, "route_denied req-1")
		}, Fail, "route_denied"},
		{"git push fails authentication instead of refusing it", "git-push", func(t *testing.T, o *ReadOptions) {
			o.GitPush = gitStub(t, "git-receive-pack", http.StatusUnauthorized, "authentication_failed req-1")
		}, Fail, "authentication_failed"},
		{"git push answers 500", "git-push", func(t *testing.T, o *ReadOptions) {
			o.GitPush = gitStub(t, "git-receive-pack", http.StatusInternalServerError, "")
		}, Fail, "status 500"},
		{"git push probes the wrong service", "git-push", func(t *testing.T, o *ReadOptions) {
			o.GitPush = gitStub(t, "git-upload-pack", http.StatusUnauthorized, "")
		}, Fail, "status 400"},
		{"kagent is not requested", "kagent", func(t *testing.T, o *ReadOptions) {}, Skip, "not requested"},
		{"kagent probe fails", "kagent", func(t *testing.T, o *ReadOptions) {
			o.Kagent = func(context.Context) (string, error) { return "", errors.New("agent UNAVAILABLE") }
		}, Fail, "agent UNAVAILABLE"},
		{"kagent probe passes", "kagent", func(t *testing.T, o *ReadOptions) {
			o.Kagent = func(context.Context) (string, error) { return "agent ns/main: found and ready", nil }
		}, OK, "found and ready"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := healthyOptions(t)
			tc.mutate(t, &opts)
			c := findCheck(t, runReadOnly(opts), tc.check)
			if c.Status != tc.want || !strings.Contains(c.Detail, tc.detail) {
				t.Errorf("%s = %s, want %s with detail containing %q", tc.check, c, tc.want, tc.detail)
			}
		})
	}
}

func TestSummaryCountsAndFlagsFailure(t *testing.T) {
	var out strings.Builder
	failed := Summary(&out, []Check{
		{Name: "a", Status: OK}, {Name: "b", Status: Skip}, {Name: "c", Status: Fail, Detail: "boom"},
	})
	if !failed {
		t.Error("Summary did not flag the failed check")
	}
	if got := out.String(); got != "summary: 1 ok, 1 fail, 1 skip\n" {
		t.Errorf("summary = %q", got)
	}
	if Summary(io.Discard, []Check{{Name: "a", Status: OK}}) {
		t.Error("Summary flagged a run with no failures")
	}
}

func TestCheckLineFormat(t *testing.T) {
	if got := okCheck("health", "status=healthy").String(); got != "ok health status=healthy" {
		t.Errorf("ok line = %q", got)
	}
	if got := skipCheck("git-read", "transport off").String(); got != "skip git-read transport off" {
		t.Errorf("skip line = %q", got)
	}
	if got := failCheck("mcp", errors.New("status 200")).String(); got != "fail mcp status 200" {
		t.Errorf("fail line = %q", got)
	}
}

// TestDecodeIsStrictAboutStatus guards the shared caller: a 2xx body still has to decode, and a non-2xx
// body is never decoded into the contract type.
func TestDecodeIsStrictAboutStatus(t *testing.T) {
	api := stubAPI(t, map[string]http.HandlerFunc{"/attention": statusHandler(404, `{"count":3}`)})
	var out struct{ Count int }
	err := newCaller("", 5*time.Second).getJSON(context.Background(), api+"/attention", &out)
	if err == nil || out.Count != 0 {
		t.Fatalf("getJSON on 404 = %v, count %d; want an error and no decode", err, out.Count)
	}
}

// A kagent error can span lines, and the check line must still be one line that keeps every part of it.
func TestFailedCheckStaysOnOneLine(t *testing.T) {
	err := errors.Join(errors.New("agent UNAVAILABLE"), errors.New("no ready pod"))
	got := failCheck("kagent", err).String()
	if strings.Contains(got, "\n") {
		t.Errorf("check line spans lines: %q", got)
	}
	if !strings.HasSuffix(got, "agent UNAVAILABLE; no ready pod") {
		t.Errorf("check line = %q, want both error lines after the name", got)
	}
}

// Only a plain code word is echoed from a refusal body; anything else stays out of the check line.
func TestRefusalCodeIsOnlyAPlainWord(t *testing.T) {
	tests := map[string]string{
		"host_denied req-1\n":           "host_denied",
		"authentication_required req-1": "authentication_required",
		"":                              "",
		"<b>hi</b> req-1":               "",
		"Host: evil.example":            "",
	}
	for body, want := range tests {
		if got := refusalCode(body); got != want {
			t.Errorf("refusalCode(%q) = %q, want %q", body, got, want)
		}
	}
}

// hostRecorder answers a git discovery or MCP request the way a healthy gate does, and records the Host
// header of each request it gets.
type hostRecorder struct {
	srv   *httptest.Server
	mu    sync.Mutex
	hosts []string
}

func newHostRecorder(t *testing.T, status int, body string) *hostRecorder {
	t.Helper()
	rec := &hostRecorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.hosts = append(rec.hosts, r.Host)
		rec.mu.Unlock()
		statusHandler(status, body)(w, r)
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (r *hostRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hosts...)
}

func TestProbesSendTheConfiguredHost(t *testing.T) {
	push := newHostRecorder(t, http.StatusUnauthorized, "authentication_required req-1")
	read := newHostRecorder(t, http.StatusUnauthorized, "")
	mcp := newHostRecorder(t, http.StatusUnauthorized, "")
	opts := healthyOptions(t)
	opts.GitPush, opts.GitRead, opts.MCP = push.srv.URL, read.srv.URL, mcp.srv.URL
	opts.GitPushHost = "mainloop-git-push.ns.svc.cluster.local"
	opts.GitReadHost = "mainloop-git-read.ns.svc.cluster.local"
	opts.MCPHost = "mainloop-mcp.ns.svc.cluster.local"

	checks := runReadOnly(opts)
	for name, want := range map[string]struct {
		rec  *hostRecorder
		host string
	}{
		"git-push": {push, opts.GitPushHost},
		"git-read": {read, opts.GitReadHost},
		"mcp":      {mcp, opts.MCPHost},
	} {
		if c := findCheck(t, checks, name); c.Status != OK {
			t.Errorf("%s = %v, want ok", name, c)
		}
		if got := want.rec.seen(); len(got) != 1 || got[0] != want.host {
			t.Errorf("%s listener saw Host %q, want exactly [%q]", name, got, want.host)
		}
	}
}

func TestProbesKeepTheURLHostWhenNoneIsConfigured(t *testing.T) {
	push := newHostRecorder(t, http.StatusUnauthorized, "authentication_required req-1")
	read := newHostRecorder(t, http.StatusUnauthorized, "")
	mcp := newHostRecorder(t, http.StatusUnauthorized, "")
	opts := healthyOptions(t)
	opts.GitPush, opts.GitRead, opts.MCP = push.srv.URL, read.srv.URL, mcp.srv.URL

	runReadOnly(opts)
	for name, rec := range map[string]*hostRecorder{"git-push": push, "git-read": read, "mcp": mcp} {
		want := strings.TrimPrefix(rec.srv.URL, "http://")
		if got := rec.seen(); len(got) != 1 || got[0] != want {
			t.Errorf("%s listener saw Host %q, want exactly [%q]", name, got, want)
		}
	}
}
