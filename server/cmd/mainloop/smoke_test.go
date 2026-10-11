package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// countingServer counts the requests it receives, so a refused command can be shown to send none.
func countingServer(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &hits
}

func TestSmokeRefusesBeforeAnyRequest(t *testing.T) {
	url, hits := countingServer(t)
	liveTargets := []string{
		"--base-url", url, "--git-push-url", url, "--project-id", "p1", "--repo", "oldsj/smoke-repo",
		"--app-login", "mainloop-app", "--provider", "claude",
	}
	tests := []struct {
		name   string
		args   []string
		stderr string // substring the refusal must print
	}{
		{"no mode", []string{"--base-url", url}, "choose exactly one of --read-only or --live"},
		{"both modes", []string{"--read-only", "--live", "--budget", "1"}, "choose exactly one of --read-only or --live"},
		{"live without a budget", append([]string{"--live", "--merge-tools", "off"}, liveTargets...), "--budget N"},
		{"live budget above the cap", append([]string{"--live", "--budget", "4", "--merge-tools", "off"}, liveTargets...), "--budget must be from 1 to 3"},
		{"live without merge tools stated", append([]string{"--live", "--budget", "1"}, liveTargets...), "--merge-tools must be on or off"},
		{"live without a base URL", []string{"--live", "--budget", "1", "--merge-tools", "on", "--git-push-url", url}, "--base-url is required"},
		{"live with a read-only flag", append([]string{"--live", "--budget", "1", "--merge-tools", "on", "--namespace", "ns"}, liveTargets...), "--namespace applies to --read-only only"},
		{"read-only with a live flag", []string{"--read-only", "--base-url", url, "--budget", "1"}, "--budget applies to --live only"},
		{"read-only with no target", []string{"--read-only", "--namespace", ""}, "--base-url is required"},
		{"read-only with a non-HTTP URL", []string{"--read-only", "--base-url", "ftp://example", "--git-push-url", url}, "absolute http or https URL"},
		{"unknown flag", []string{"--read-only", "--bogus"}, "flag provided but not defined"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if got := runSmoke(context.Background(), tc.args, &out, &errb); got != exitUsage {
				t.Errorf("exit = %d, want %d; stderr %q", got, exitUsage, errb.String())
			}
			if !strings.Contains(errb.String(), tc.stderr) {
				t.Errorf("stderr = %q, want it to contain %q", errb.String(), tc.stderr)
			}
		})
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("refused commands sent %d request(s), want none", n)
	}
}

// smokeListener answers every listener path the read-only run reads, all from one server.
func smokeListener(t *testing.T, projectsStatus int) string {
	t.Helper()
	mux := http.NewServeMux()
	json := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		}
	}
	mux.HandleFunc("/health", json(`{"status":"healthy","database":"ok"}`))
	mux.HandleFunc("/projects", func(w http.ResponseWriter, r *http.Request) {
		if projectsStatus != http.StatusOK {
			w.WriteHeader(projectsStatus)
			return
		}
		json(`[]`)(w, r)
	})
	mux.HandleFunc("/sessions", json(`[]`))
	mux.HandleFunc("/workspaces", json(`[]`))
	mux.HandleFunc("/attention", json(`{"count":0,"items":[],"sessions":[]}`))
	mux.HandleFunc("/main-thread", json(`{"mode":"native"}`))
	mux.HandleFunc("/events", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: connected\ndata: {}\n\n")
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	mux.HandleFunc("/smoke/probe.git/info/refs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestSmokeReadOnlyPrintsOneLinePerCheck(t *testing.T) {
	url := smokeListener(t, http.StatusOK)
	var out, errb bytes.Buffer
	code := runSmoke(context.Background(), []string{
		"--read-only", "--base-url", url, "--mcp-url", url, "--git-url", url, "--git-push-url", url,
	}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stdout %q stderr %q", code, out.String(), errb.String())
	}
	for _, want := range []string{
		"ok health status=healthy database=ok",
		"ok mcp 401 without a bearer",
		"ok git-push 401 without a capability",
		"skip kagent not requested",
		"summary: 10 ok, 0 fail, 1 skip",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, out.String())
		}
	}
}

func TestSmokeReadOnlyExitsNonZeroOnAFailedCheck(t *testing.T) {
	url := smokeListener(t, http.StatusInternalServerError)
	var out, errb bytes.Buffer
	code := runSmoke(context.Background(), []string{
		"--read-only", "--base-url", url, "--mcp-url", url, "--git-url", url, "--git-push-url", url,
	}, &out, &errb)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(out.String(), "fail projects GET /projects: status 500") {
		t.Errorf("stdout lacks the failed projects line:\n%s", out.String())
	}
}
