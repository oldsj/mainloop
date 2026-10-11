package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/config"
)

func testConfig(t *testing.T, extra map[string]string) *config.Config {
	t.Helper()
	env := map[string]string{"AGENT_TOKEN_KEY": "test-agent-token-key-0123456789ab"}
	for k, v := range extra {
		env[k] = v
	}
	cfg, err := config.Parse(env)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestCredentialsOutsideAClusterCannotIssue(t *testing.T) {
	creds, err := newCredentialsWith(testConfig(t, nil), nil, quiet, func(string) (agentauth.SecretStore, error) {
		t.Fatal("asked for a cluster client outside a cluster")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Create is the first Kubernetes call of an Issue; nothing here needs a database to see it fail.
	if _, err := creds.Secrets.Create(context.Background(), "n", "mcp", "b"); !errors.Is(err, agentauth.ErrNotConfigured) {
		t.Errorf("Create err = %v, want ErrNotConfigured", err)
	}
	if _, ok := creds.Secrets.(agentauth.Unconfigured); !ok {
		t.Errorf("store = %T", creds.Secrets)
	}
}

func TestCredentialsInAClusterNeedAWorkingClient(t *testing.T) {
	cfg := testConfig(t, map[string]string{"KUBERNETES_SERVICE_HOST": "10.0.0.1"})
	boom := errors.New("no service account")
	if _, err := newCredentialsWith(cfg, nil, quiet, func(string) (agentauth.SecretStore, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want startup to fail", err)
	}
	var asked string
	creds, err := newCredentialsWith(cfg, nil, quiet, func(ns string) (agentauth.SecretStore, error) {
		asked = ns
		return agentauth.Unconfigured{}, nil
	})
	if err != nil || creds == nil || asked != cfg.Kagent.Namespace {
		t.Fatalf("creds = %v, namespace %q, err %v", creds, asked, err)
	}
}
