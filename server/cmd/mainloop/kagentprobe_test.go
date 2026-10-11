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
		GatewayURL: url, UserID: "mainloop", Namespace: "kagent", MainAgent: "mainloop-main",
		RequestTimeout: config.Seconds(5 * time.Second), TurnTimeout: config.Seconds(5 * time.Second),
		SendRetryBudget: config.Seconds(time.Second),
	}
}

func TestKagentProbeReportsTheAgentAndSessions(t *testing.T) {
	srv := kagenttest.New()
	defer srv.Close()
	srv.AddAgent("kagent", "mainloop-main")

	var out bytes.Buffer
	if err := kagentProbe(context.Background(), probeSettings(srv.URL), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"agent kagent/mainloop-main: found", "sessions visible: 0", "x-user-id mainloop"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q lacks %q", out.String(), want)
		}
	}
	if n := len(srv.Requests()); n != 2 {
		t.Errorf("probe sent %d requests, want a GetAgent and a ListSessions", n)
	}
}

func TestKagentProbeFailsWhenTheAgentIsMissing(t *testing.T) {
	srv := kagenttest.New()
	defer srv.Close()
	var out bytes.Buffer
	err := kagentProbe(context.Background(), probeSettings(srv.URL), &out)
	if err == nil || !strings.Contains(err.Error(), "get agent kagent/mainloop-main") {
		t.Fatalf("got %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q on failure", out.String())
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

func TestKagentProbeCommandNeedsSettings(t *testing.T) {
	t.Setenv("AGENT_TOKEN_KEY", "")
	var out, errb bytes.Buffer
	if got := run([]string{"kagent-probe"}, &out, &errb); got != exitFailure {
		t.Fatalf("exit = %d", got)
	}
	if !strings.Contains(errb.String(), "kagent-probe:") {
		t.Errorf("stderr %q", errb.String())
	}
}
