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

// kagentProbe checks that kagent answers: it reads both main agents (Claude and Codex, so a missing
// or unprepared Codex main is reported before the owner selects it) and lists the Sessions this
// identity can see. An agent that can create Sessions has a ready revision (kagent.AgentReady). Each
// agent gets a line, and every problem is reported before the probe fails. It only reads.
func kagentProbe(ctx context.Context, cfg config.Kagent, stdout io.Writer) error {
	client, err := kagent.New(kagent.FromSettings(cfg))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	_, _ = fmt.Fprintf(stdout, "gateway %s as %s\n", cfg.GatewayURL, probeIdentity(cfg))
	var problems []error
	for _, name := range []string{cfg.MainAgent, cfg.MainCodexAgent} {
		ref := kagent.AgentRef{Namespace: cfg.Namespace, Name: name}
		agent, err := client.GetAgent(ctx, ref)
		switch {
		case err != nil:
			err = fmt.Errorf("get agent %s/%s: %w", ref.Namespace, ref.Name, err)
		case agent.GetRef().GetName() == "":
			err = fmt.Errorf("get agent %s/%s: reply names no agent", ref.Namespace, ref.Name)
		case !kagent.AgentReady(agent):
			err = fmt.Errorf("agent %s/%s has no ready revision, so it cannot create Sessions", ref.Namespace, ref.Name)
		}
		if err != nil {
			_, _ = fmt.Fprintf(stdout, "agent %s/%s: UNAVAILABLE\n", ref.Namespace, ref.Name)
			problems = append(problems, err)
			continue
		}
		_, _ = fmt.Fprintf(stdout, "agent %s/%s: found and ready\n", ref.Namespace, ref.Name)
	}
	if len(problems) > 0 {
		return errors.Join(problems...)
	}
	sessions, err := client.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
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
