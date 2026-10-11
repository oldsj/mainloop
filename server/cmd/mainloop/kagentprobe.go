package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/kagent"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

const probeTimeout = time.Minute

// kagentProbe checks that kagent answers: it reads both main agents (Claude and Codex, so a missing
// Codex main is reported before the owner selects it) and lists the Sessions this identity can see.
// It only reads.
func kagentProbe(ctx context.Context, cfg config.Kagent, stdout io.Writer) error {
	client, err := kagent.New(kagent.FromSettings(cfg))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	var agents []*kagentapi.Agent
	for _, name := range []string{cfg.MainAgent, cfg.MainCodexAgent} {
		ref := kagent.AgentRef{Namespace: cfg.Namespace, Name: name}
		agent, err := client.GetAgent(ctx, ref)
		if err != nil {
			return fmt.Errorf("get agent %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		if agent.GetRef().GetName() == "" {
			return errors.New("get agent: reply names no agent")
		}
		agents = append(agents, agent)
	}
	sessions, err := client.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	_, _ = fmt.Fprintf(stdout, "gateway %s as %s\n", cfg.GatewayURL, probeIdentity(cfg))
	for _, agent := range agents {
		_, _ = fmt.Fprintf(stdout, "agent %s/%s: found\n", agent.GetRef().GetNamespace(), agent.GetRef().GetName())
	}
	_, _ = fmt.Fprintf(stdout, "sessions visible: %d\n", len(sessions))
	return nil
}

func probeIdentity(cfg config.Kagent) string {
	if cfg.ControlTokenFile != "" {
		return "bearer token"
	}
	return "x-user-id " + cfg.UserID
}

// runKagentProbe loads only the kagent settings and runs the probe.
func runKagentProbe(ctx context.Context, stdout, stderr io.Writer) int {
	cfg, err := config.LoadKagent()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kagent-probe: %v\n", err)
		return exitFailure
	}
	if err := kagentProbe(ctx, *cfg, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "kagent-probe: %v\n", err)
		return exitFailure
	}
	return 0
}
