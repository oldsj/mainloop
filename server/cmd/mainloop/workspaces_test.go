package main

import (
	"bufio"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/pushgate"
	"github.com/oldsj/mainloop/server/internal/workspaces"
)

// prodEnv reads the production-shaped config fixture; a value "@file" is replaced by that file.
func prodEnv(t *testing.T) map[string]string {
	t.Helper()
	dir := filepath.Join("..", "..", "internal", "config", "testdata")
	f, err := os.Open(filepath.Join(dir, "prod.env"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	vars := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		if file, ok := strings.CutPrefix(v, "@"); ok {
			b, err := os.ReadFile(filepath.Join(dir, file))
			if err != nil {
				t.Fatal(err)
			}
			v = string(b)
		}
		vars[k] = v
	}
	return vars
}

// serve-api gives the workspace service its push path exactly when the push gate is on.
func TestWorkspaceOptionsWireGitPushOnlyWithTheGate(t *testing.T) {
	gate := pushgate.NewService(nil, nil, nil, nil)
	gitRead := &workspaces.GitRead{}
	for name, tc := range map[string]struct {
		set map[string]string
		on  bool
	}{
		"gate on":         {nil, true},
		"gate switch off": {map[string]string{"PUSH_GATE_ENABLED": "false"}, false},
		"transport off":   {map[string]string{"GIT_TRANSPORT_ENABLED": "false"}, false},
		"no GitHub App":   {map[string]string{"GITHUB_APP_ID": "", "GITHUB_APP_PRIVATE_KEY": ""}, false},
	} {
		t.Run(name, func(t *testing.T) {
			vars := prodEnv(t)
			for k, v := range tc.set {
				vars[k] = v
			}
			cfg, err := config.Parse(vars)
			if err != nil {
				t.Fatal(err)
			}
			svc := workspaces.New(cfg, nil, nil, nil, nil, nil, nil, slog.Default(), workspaceOptions(cfg, gate, gitRead)...)
			if svc.GitRead != gitRead {
				t.Errorf("GitRead = %p, want the one passed in", svc.GitRead)
			}
			if !tc.on {
				if svc.GitPush != nil {
					t.Fatalf("GitPush = %+v with the gate off", svc.GitPush)
				}
				return
			}
			if svc.GitPush == nil || svc.GitPush.Gate != gate || svc.GitPush.Origin != cfg.Git.PushOrigin {
				t.Fatalf("GitPush = %+v, want the process gate and origin %q", svc.GitPush, cfg.Git.PushOrigin)
			}
		})
	}
}
