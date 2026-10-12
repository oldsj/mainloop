package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
)

func probeSettings(url string) config.Kagent {
	return config.Kagent{
		GatewayURL: url, UserID: "mainloop", Namespace: "kagent", MainAgent: "mainloop-main", MainCodexAgent: "mainloop-main-codex",
		RequestTimeout: config.Seconds(5 * time.Second), TurnTimeout: config.Seconds(5 * time.Second),
		SendRetryBudget: config.Seconds(time.Second),
	}
}

func TestKagentProbeReportsTheAgentAndSessions(t *testing.T) {
	srv := kagenttest.New()
	defer srv.Close()
	srv.AddAgent("kagent", "mainloop-main")
	srv.AddAgent("kagent", "mainloop-main-codex")

	var out bytes.Buffer
	if err := kagentProbe(context.Background(), probeSettings(srv.URL), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"agent kagent/mainloop-main: found", "agent kagent/mainloop-main-codex: found", "sessions visible: 0", "x-user-id mainloop"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q lacks %q", out.String(), want)
		}
	}
	if n := len(srv.Requests()); n != 3 {
		t.Errorf("probe sent %d requests, want two GetAgents and a ListSessions", n)
	}
}

func TestKagentProbeReportsBothAgentsWhenBothAreMissing(t *testing.T) {
	srv := kagenttest.New()
	defer srv.Close()
	var out bytes.Buffer
	err := kagentProbe(context.Background(), probeSettings(srv.URL), &out)
	if err == nil || !strings.Contains(err.Error(), "get agent kagent/mainloop-main:") || !strings.Contains(err.Error(), "get agent kagent/mainloop-main-codex:") {
		t.Fatalf("got %v, want both agents named", err)
	}
	for _, want := range []string{"agent kagent/mainloop-main: UNAVAILABLE", "agent kagent/mainloop-main-codex: UNAVAILABLE"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q lacks %q", out.String(), want)
		}
	}
}

func TestKagentProbeFailsWhenTheCodexMainIsMissing(t *testing.T) {
	srv := kagenttest.New()
	defer srv.Close()
	srv.AddAgent("kagent", "mainloop-main")
	var out bytes.Buffer
	err := kagentProbe(context.Background(), probeSettings(srv.URL), &out)
	if err == nil || !strings.Contains(err.Error(), "get agent kagent/mainloop-main-codex") || strings.Contains(err.Error(), "kagent/mainloop-main:") {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(out.String(), "agent kagent/mainloop-main: found") || !strings.Contains(out.String(), "agent kagent/mainloop-main-codex: UNAVAILABLE") {
		t.Errorf("output %q", out.String())
	}
}

// An Agent that exists but has no ready revision cannot create Sessions, so it is not "found".
func TestKagentProbeFailsForAnAgentThatIsNotReady(t *testing.T) {
	srv := kagenttest.New()
	defer srv.Close()
	srv.AddAgentNotReady("kagent", "mainloop-main")
	srv.AddAgentNotReady("kagent", "mainloop-main-codex")
	var out bytes.Buffer
	err := kagentProbe(context.Background(), probeSettings(srv.URL), &out)
	if err == nil || strings.Count(err.Error(), "no ready revision") != 2 {
		t.Fatalf("got %v, want both agents reported as not ready", err)
	}
	if strings.Contains(out.String(), "found") || !strings.Contains(out.String(), "agent kagent/mainloop-main-codex: UNAVAILABLE") {
		t.Errorf("output %q", out.String())
	}
}

func TestKagentProbeFailsWhenTheGatewayIsDown(t *testing.T) {
	srv := kagenttest.New()
	url := srv.URL
	srv.Close()
	if err := kagentProbe(context.Background(), probeSettings(url), &bytes.Buffer{}); err == nil {
		t.Fatal("probe succeeded against a closed port")
	}
}

func TestKagentProbeCommandReadsOnlyKagentSettings(t *testing.T) {
	srv := kagenttest.New()
	defer srv.Close()
	srv.AddAgent("kagent", "mainloop-main")
	srv.AddAgent("kagent", "mainloop-main-codex")
	t.Setenv("AGENT_TOKEN_KEY", "")
	t.Setenv("MAINLOOP_OWNER_ID", "")
	t.Setenv("KAGENT_GATEWAY_URL", srv.URL)
	var out, errb bytes.Buffer
	if got := run([]string{"kagent-probe"}, &out, &errb); got != 0 {
		t.Fatalf("exit = %d, stderr %q", got, errb.String())
	}
	if !strings.Contains(out.String(), "agent kagent/mainloop-main: found") {
		t.Errorf("stdout %q", out.String())
	}
}

func TestKagentProbeCommandRejectsBadSettings(t *testing.T) {
	t.Setenv("KAGENT_CONTROL_TOKEN_FILE", "/nonexistent/token")
	var out, errb bytes.Buffer
	if got := run([]string{"kagent-probe"}, &out, &errb); got != exitFailure {
		t.Fatalf("exit = %d", got)
	}
	if !strings.Contains(errb.String(), "kagent-probe:") || !strings.Contains(errb.String(), "KAGENT_CONTROL_TOKEN_FILE") {
		t.Errorf("stderr %q", errb.String())
	}
}
