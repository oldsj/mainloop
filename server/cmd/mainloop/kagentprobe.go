package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/kagent"
)

const probeTimeout = time.Minute

// kagentProbe checks that kagent answers: it reads the configured main agent and lists the
// Sessions this identity can see. It only reads.
func kagentProbe(ctx context.Context, cfg config.Kagent, stdout io.Writer) error {
	client, err := kagent.New(kagent.FromSettings(cfg))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	ref := kagent.AgentRef{Namespace: cfg.Namespace, Name: cfg.MainAgent}
	agent, err := client.GetAgent(ctx, ref)
	if err != nil {
		return fmt.Errorf("get agent %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	sessions, err := client.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	if agent.GetRef().GetName() == "" {
		return errors.New("get agent: reply names no agent")
	}
	_, _ = fmt.Fprintf(stdout, "gateway %s as %s\n", cfg.GatewayURL, probeIdentity(cfg))
	_, _ = fmt.Fprintf(stdout, "agent %s/%s: found\n", agent.GetRef().GetNamespace(), agent.GetRef().GetName())
	_, _ = fmt.Fprintf(stdout, "sessions visible: %d\n", len(sessions))
	return nil
}

func probeIdentity(cfg config.Kagent) string {
	if cfg.ControlTokenFile != "" {
		return "bearer token"
	}
	return "x-user-id " + cfg.UserID
}

// runKagentProbe loads the settings and runs the probe. config.Load needs AGENT_TOKEN_KEY even
// though the probe does not use it; any nonblank value will do.
func runKagentProbe(ctx context.Context, stdout, stderr io.Writer) int {
	cfg, err := config.Load()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kagent-probe: %v\n", err)
		return exitFailure
	}
	if err := kagentProbe(ctx, cfg.Kagent, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "kagent-probe: %v\n", err)
		return exitFailure
	}
	return 0
}
