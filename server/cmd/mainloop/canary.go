package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/oldsj/mainloop/server/internal/canary"
	"github.com/oldsj/mainloop/server/internal/config"
)

// targetsFlag is a repeatable name=url[@host] flag.
type targetsFlag []canary.Options

func (t *targetsFlag) String() string { return fmt.Sprint(len(*t)) }

func (t *targetsFlag) Set(v string) error {
	name, rest, ok := strings.Cut(v, "=")
	if !ok || name == "" || rest == "" {
		return fmt.Errorf("want name=url[@host], got %q", v)
	}
	u, host, _ := strings.Cut(rest, "@")
	*t = append(*t, canary.Options{Name: name, API: strings.TrimRight(u, "/"), Host: host})
	return nil
}

// runCanary runs one synthetic turn against each target and posts one ntfy alert for the ones
// that failed or were slow. Exit 1 when any failed.
func runCanary(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("canary", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var targets targetsFlag
	fs.Var(&targets, "target", "name=url[@host] of an owner API to check; repeatable (default prod=in-cluster mainloop-backend)")
	model := fs.String("model", "claude-haiku-5-5", "model for the canary session, set at create")
	slow := fs.Duration("slow", 90*time.Second, "reply time above which a run is reported slow")
	deadline := fs.Duration("deadline", 4*time.Minute, "how long to wait for the reply")
	poll := fs.Duration("poll", 3*time.Second, "pause between conversation reads")
	timeout := fs.Duration("timeout", 15*time.Second, "deadline for each request")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if len(targets) == 0 {
		u := inClusterURL(defaultNamespace(), "mainloop-backend", 8000)
		if u == "" {
			_, _ = fmt.Fprintln(stderr, "canary: outside the cluster, pass --target")
			return exitUsage
		}
		targets = append(targets, canary.Options{Name: "prod", API: u})
	}
	nt, err := config.LoadNtfy()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitUsage
	}

	results := make([]canary.Result, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		t.Model, t.Slow, t.Deadline, t.Poll, t.Request = *model, *slow, *deadline, *poll, *timeout
		wg.Go(func() { results[i] = canary.Run(ctx, t) })
	}
	wg.Wait()

	failed := false
	for _, r := range results {
		_, _ = fmt.Fprintln(stdout, r.String())
		failed = failed || r.Err != nil
	}
	if nt.URL != "" {
		if err := (canary.Ntfy{URL: nt.URL, Topic: nt.Topic, Token: nt.Token.Reveal()}).Alert(ctx, results); err != nil {
			_, _ = fmt.Fprintln(stderr, "canary: alert:", err)
			return exitFailure
		}
	}
	if failed {
		return exitFailure
	}
	return 0
}
