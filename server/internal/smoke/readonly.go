package smoke

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
)

// ReadOptions configures the read-only checks. Kagent is nil unless the operator asked for the kagent
// probe, which is only meaningful from inside the cluster.
type ReadOptions struct {
	Endpoints
	APIHost string

	// Host headers for the probes, for port-forwards to a non-dev instance. Empty keeps the URL's host.
	MCPHost     string
	GitReadHost string
	GitPushHost string

	Timeout time.Duration
	Kagent  func(ctx context.Context) (string, error)

	// eventsWait bounds the wait for the connected frame. Zero means eventsDeadline; tests shorten it.
	eventsWait time.Duration
}

// eventsDeadline bounds the wait for the SSE connected frame. The server sends it at once, so a longer
// wait means the stream is not working.
const eventsDeadline = 10 * time.Second

// probeRepo is a repository that need not exist: the transport refuses an unauthenticated request
// before it looks the repository up, so the probe never names a real one.
const probeRepo = "/smoke/probe"

// ReadOnly runs the read-only checks in order and passes each result to emit as it completes. It sends
// no writes and needs no credentials: the owner API has no per-request auth, and the capability-gated
// listeners are asked for a refusal with no credential attached.
func ReadOnly(ctx context.Context, opts ReadOptions, emit func(Check)) {
	c := newCaller(opts.APIHost, opts.Timeout)
	base := opts.API
	eventsWait := opts.eventsWait
	if eventsWait == 0 {
		eventsWait = eventsDeadline
	}
	for _, check := range []func() Check{
		func() Check { return checkHealth(ctx, c, base) },
		func() Check {
			return readCheck(ctx, c, "projects", base+"/projects", func(v []api.Project) string { return count(len(v)) })
		},
		func() Check {
			return readCheck(ctx, c, "sessions", base+"/sessions", func(v []api.Session) string { return count(len(v)) })
		},
		func() Check {
			return readCheck(ctx, c, "workspaces", base+"/workspaces", func(v []api.WorkspaceLifecycle) string {
				return count(len(v))
			})
		},
		func() Check {
			return readCheck(ctx, c, "attention", base+"/attention", func(v api.AttentionSnapshot) string {
				return count(v.Count)
			})
		},
		func() Check {
			return readCheck(ctx, c, "main-thread", base+"/main-thread", func(v api.MainThreadInfo) string {
				return "mode=" + v.Mode
			})
		},
		func() Check { return checkEvents(ctx, c, base, eventsWait) },
		func() Check { return checkMCP(ctx, c, opts.MCP, opts.MCPHost) },
		func() Check { return checkGitRead(ctx, c, opts.GitRead, opts.GitReadHost) },
		func() Check { return checkGitPush(ctx, c, opts.GitPush, opts.GitPushHost) },
		func() Check { return checkKagent(ctx, opts.Kagent) },
	} {
		emit(check())
	}
}

func count(n int) string { return fmt.Sprintf("count=%d", n) }

func checkHealth(ctx context.Context, c caller, base string) Check {
	var h api.HealthResponse
	if err := c.getJSON(ctx, base+"/health", &h); err != nil {
		return failCheck("health", err)
	}
	if h.Status != api.Healthy {
		return failCheck("health", fmt.Errorf("status=%s database=%s, want healthy", h.Status, h.Database))
	}
	return okCheck("health", fmt.Sprintf("status=%s database=%s", h.Status, h.Database))
}

// readCheck GETs one list or snapshot endpoint and checks that its body decodes into the contract type.
func readCheck[T any](ctx context.Context, c caller, name, target string, describe func(T) string) Check {
	var v T
	if err := c.getJSON(ctx, target, &v); err != nil {
		return failCheck(name, err)
	}
	return okCheck(name, describe(v))
}

// checkEvents reads the SSE stream until the connected frame arrives, for at most wait. The stream never
// ends on its own, so the request context is what stops it.
func checkEvents(ctx context.Context, c caller, base string, wait time.Duration) Check {
	const name = "events"
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
	if err != nil {
		return failCheck(name, err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.host != "" {
		req.Host = c.host
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return failCheck(name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return failCheck(name, fmt.Errorf("status %d", resp.StatusCode))
	}
	scan := bufio.NewScanner(resp.Body)
	for scan.Scan() {
		if scan.Text() == "event: connected" {
			return okCheck(name, "connected frame received")
		}
	}
	if err := ctx.Err(); err != nil {
		return failCheck(name, fmt.Errorf("no connected frame within %s", wait))
	}
	if err := scan.Err(); err != nil {
		return failCheck(name, err)
	}
	return failCheck(name, fmt.Errorf("stream ended before a connected frame"))
}

// checkMCP posts with no bearer. The listener must refuse before it reads the body, so the body is empty.
func checkMCP(ctx context.Context, c caller, base, host string) Check {
	const name = "mcp"
	header := http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json, text/event-stream"}}
	status, _, err := c.probe(ctx, http.MethodPost, base+"/mcp", host, header)
	if err != nil {
		return failCheck(name, err)
	}
	if status != http.StatusUnauthorized {
		return failCheck(name, fmt.Errorf("status %d without a bearer, want 401", status))
	}
	return okCheck(name, "401 without a bearer")
}

// checkGitRead asks for upload-pack discovery with no capability. A disabled transport answers 501 on
// every path except /health, which is how the probe tells "off" from "broken".
func checkGitRead(ctx context.Context, c caller, base, host string) Check {
	const name = "git-read"
	status, _, err := c.probe(ctx, http.MethodGet, base+probeRepo+".git/info/refs?service=git-upload-pack", host, nil)
	if err != nil {
		return failCheck(name, err)
	}
	switch status {
	case http.StatusNotImplemented:
		return skipCheck(name, "transport off")
	case http.StatusUnauthorized, http.StatusForbidden:
		return okCheck(name, fmt.Sprintf("%d without a capability", status))
	}
	return failCheck(name, fmt.Errorf("status %d without a capability, want 401 or 403", status))
}

// checkGitPush asks for receive-pack discovery with no capability. A disabled gate refuses with 403 and
// git_transport_disabled. An enabled gate refuses the missing capability with 401 authentication_required.
// Any other refusal means the request stopped before that check (a Host, path or route refusal), so the
// check fails and names the code.
func checkGitPush(ctx context.Context, c caller, base, host string) Check {
	const name = "git-push"
	status, body, err := c.probe(ctx, http.MethodGet, base+probeRepo+".git/info/refs?service=git-receive-pack", host, nil)
	if err != nil {
		return failCheck(name, err)
	}
	code := refusalCode(body)
	switch {
	case status == http.StatusForbidden && code == "git_transport_disabled":
		return skipCheck(name, "transport off")
	case status == http.StatusUnauthorized && code == "authentication_required":
		return okCheck(name, "401 authentication_required without a capability")
	}
	got := strings.TrimSpace(fmt.Sprintf("%d %s", status, code))
	return failCheck(name, fmt.Errorf("status %s, want 401 authentication_required", got))
}

// refusalCodePattern matches the code the push gate writes first in a refusal body.
var refusalCodePattern = regexp.MustCompile(`^[a-z_]{1,64}$`)

// refusalCode returns the first word of a refusal body when it is a plain code. Anything else is not
// echoed, so a stray body cannot put text into the check line.
func refusalCode(body string) string {
	word, _, _ := strings.Cut(strings.TrimSpace(body), " ")
	if !refusalCodePattern.MatchString(word) {
		return ""
	}
	return word
}

func checkKagent(ctx context.Context, probe func(context.Context) (string, error)) Check {
	const name = "kagent"
	if probe == nil {
		return skipCheck(name, "not requested (--kagent runs it in-cluster)")
	}
	detail, err := probe(ctx)
	if err != nil {
		return failCheck(name, err)
	}
	return okCheck(name, detail)
}
