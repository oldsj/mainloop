package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/smoke"
)

const (
	exitUsage      = 2
	exitIncomplete = 3 // a live run passed its checks but skipped a step, so completion is unproven

	serviceAccountNamespace = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

// Each mode refuses the flags that only the other mode uses, so a live-only option cannot look like it
// applied to a read-only run. Flags both modes use (base-url, git-push-url, api-host, timeout) are not listed.
var (
	smokeReadOnlyFlags = []string{"mcp-url", "git-url", "kagent", "namespace"}
	smokeLiveFlags     = []string{"budget", "project-id", "repo", "app-login", "provider", "merge-tools", "deadline", "step-deadline", "poll-interval"}
)

// runSmoke runs the operator's check of a running Mainloop: --read-only probes, or --live with a budget.
func runSmoke(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("smoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	readOnly := fs.Bool("read-only", false, "run the read-only checks: no chat turns, no writes, no quota")
	live := fs.Bool("live", false, "run a full task from chat to merge; sends chat turns and spends quota (needs --budget)")
	budget := fs.Int("budget", 0, "full live runs to start, from 1 to 3; required with --live")
	namespace := fs.String("namespace", defaultNamespace(), "read-only: namespace for the in-cluster default URLs")
	baseURL := fs.String("base-url", "", "owner API base URL (read-only default: in-cluster mainloop-backend)")
	gitPushURL := fs.String("git-push-url", "", "git push listener base URL (read-only default: in-cluster mainloop-git-push)")
	mcpURL := fs.String("mcp-url", "", "read-only: MCP listener base URL (default: in-cluster mainloop-mcp)")
	gitURL := fs.String("git-url", "", "read-only: git read listener base URL (default: in-cluster mainloop-git-read)")
	kagentProbeFlag := fs.Bool("kagent", false, "read-only: also run the kagent probe; only meaningful in the cluster")
	apiHost := fs.String("api-host", "", "Host header for owner API requests, for a port-forward")
	timeout := fs.Duration("timeout", 15*time.Second, "deadline for each request")
	projectID := fs.String("project-id", "", "live: the project to run in")
	repo := fs.String("repo", "", "live: owner/name of the disposable test repository")
	appLogin := fs.String("app-login", "", "live: the GitHub App login that must perform the merge")
	provider := fs.String("provider", "", "live: claude or codex")
	mergeTools := fs.String("merge-tools", "", "live: on or off; the API does not report whether merge tools are enabled")
	deadline := fs.Duration("deadline", 30*time.Minute, "live: overall deadline for one run")
	stepDeadline := fs.Duration("step-deadline", 10*time.Minute, "live: deadline for one step")
	pollInterval := fs.Duration("poll-interval", 5*time.Second, "live: pause between reads")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *readOnly == *live {
		_, _ = fmt.Fprintln(stderr, "smoke: choose exactly one of --read-only or --live")
		return exitUsage
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if *live {
		if !set["budget"] {
			_, _ = fmt.Fprintln(stderr, "smoke: --live needs --budget N (1 to 3 full runs); refusing to start")
			return exitUsage
		}
		if bad := firstSet(set, smokeReadOnlyFlags); bad != "" {
			_, _ = fmt.Fprintf(stderr, "smoke: --%s applies to --read-only only\n", bad)
			return exitUsage
		}
	} else if bad := firstSet(set, smokeLiveFlags); bad != "" {
		_, _ = fmt.Fprintf(stderr, "smoke: --%s applies to --live only\n", bad)
		return exitUsage
	}

	if *live {
		return runSmokeLive(ctx, stdout, stderr, liveFlags{
			baseURL: *baseURL, gitPushURL: *gitPushURL, apiHost: *apiHost, timeout: *timeout,
			projectID: *projectID, repo: *repo, appLogin: *appLogin, provider: *provider, mergeTools: *mergeTools,
			budget: *budget, deadline: *deadline, stepDeadline: *stepDeadline, pollInterval: *pollInterval,
		})
	}

	// The read-only defaults are the in-cluster Service names. Each one is replaced by its flag when set,
	// so a port-forward from a laptop only needs the flags it changes.
	for _, ep := range []struct {
		flag    *string
		service string
		port    int
	}{
		{baseURL, "mainloop-backend", 8000},
		{mcpURL, "mainloop-mcp", 80},
		{gitURL, "mainloop-git-read", 80},
		{gitPushURL, "mainloop-git-push", 80},
	} {
		if *ep.flag == "" {
			*ep.flag = inClusterURL(*namespace, ep.service, ep.port)
		}
	}
	endpoints := smoke.Endpoints{}
	for _, ep := range []struct {
		name string
		raw  string
		dst  *string
	}{
		{"--base-url", *baseURL, &endpoints.API},
		{"--mcp-url", *mcpURL, &endpoints.MCP},
		{"--git-url", *gitURL, &endpoints.GitRead},
		{"--git-push-url", *gitPushURL, &endpoints.GitPush},
	} {
		normalized, err := normalizeURL(ep.name, ep.raw)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "smoke: %v\n", err)
			return exitUsage
		}
		*ep.dst = normalized
	}

	var kagentCheck func(context.Context) (string, error)
	if *kagentProbeFlag {
		kagentCheck = runKagentCheck
	}
	var checks []smoke.Check
	smoke.ReadOnly(ctx, smoke.ReadOptions{
		Endpoints: endpoints,
		APIHost:   *apiHost,
		Timeout:   *timeout,
		Kagent:    kagentCheck,
	}, func(c smoke.Check) {
		checks = append(checks, c)
		_, _ = fmt.Fprintln(stdout, c)
	})
	if smoke.Summary(stdout, checks) {
		return exitFailure
	}
	return 0
}

type liveFlags struct {
	baseURL, gitPushURL, apiHost, projectID, repo, appLogin, provider, mergeTools string
	timeout, deadline, stepDeadline, pollInterval                                 time.Duration
	budget                                                                        int
}

// runSmokeLive refuses unless every target is named, then runs the budgeted live path. It has no
// defaults: a live run goes only where the operator pointed it.
func runSmokeLive(ctx context.Context, stdout, stderr io.Writer, f liveFlags) int {
	if f.mergeTools != "on" && f.mergeTools != "off" {
		_, _ = fmt.Fprintln(stderr, "smoke: --merge-tools must be on or off with --live")
		return exitUsage
	}
	api, err := normalizeURL("--base-url", f.baseURL)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "smoke: %v\n", err)
		return exitUsage
	}
	gitPush, err := normalizeURL("--git-push-url", f.gitPushURL)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "smoke: %v\n", err)
		return exitUsage
	}
	opts := smoke.LiveOptions{
		Endpoints:    smoke.Endpoints{API: api, GitPush: gitPush},
		APIHost:      f.apiHost,
		ProjectID:    f.projectID,
		Repo:         f.repo,
		AppLogin:     f.appLogin,
		Provider:     f.provider,
		MergeTools:   f.mergeTools == "on",
		Budget:       f.budget,
		Deadline:     f.deadline,
		StepDeadline: f.stepDeadline,
		PollInterval: f.pollInterval,
		Timeout:      f.timeout,
		GitHub:       smoke.GHCLI{},
	}
	if err := opts.Validate(); err != nil {
		_, _ = fmt.Fprintf(stderr, "smoke: %v\n", err)
		return exitUsage
	}

	var checks []smoke.Check
	smoke.Live(ctx, opts, func(c smoke.Check) {
		checks = append(checks, c)
		_, _ = fmt.Fprintln(stdout, c)
	})
	failed := smoke.Summary(stdout, checks)
	proven := smoke.CompletionProven(checks)
	_, _ = fmt.Fprintf(stdout, "live completion proven: %s\n", yesNo(proven))
	switch {
	case failed:
		return exitFailure
	case !proven:
		_, _ = fmt.Fprintln(stderr, "smoke: no run reached completion; see the skipped steps")
		return exitIncomplete
	}
	return 0
}

// runKagentCheck runs the same probe as kagent-probe, into a buffer, so its lines join the smoke output.
func runKagentCheck(ctx context.Context) (string, error) {
	cfg, err := config.LoadKagent()
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := kagentProbe(ctx, *cfg, &out); err != nil {
		return "", err
	}
	return strings.Join(strings.Split(strings.TrimSpace(out.String()), "\n"), "; "), nil
}

// normalizeURL requires an absolute http or https URL and drops a trailing slash, so paths join cleanly.
func normalizeURL(name, raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%s must be an absolute http or https URL", name)
	}
	return strings.TrimRight(raw, "/"), nil
}

func inClusterURL(namespace, service string, port int) string {
	if namespace == "" {
		return ""
	}
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", service, namespace, port)
}

// defaultNamespace reads the namespace this pod runs in. Outside the cluster it is empty, and the
// read-only run then needs its URL flags.
func defaultNamespace() string {
	raw, err := os.ReadFile(serviceAccountNamespace)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// firstSet returns the first flag in names that the operator set, or "" when none was.
func firstSet(set map[string]bool, names []string) string {
	for _, name := range names {
		if set[name] {
			return name
		}
	}
	return ""
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
