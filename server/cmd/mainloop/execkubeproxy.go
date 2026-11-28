package main

import (
	"context"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/privileged"
)

// execKubeProxy runs the sidecar of a command.exec run with cluster reach
// (docs/specs/privileged-actions.md): the executor mounts the run's credential at
// MAINLOOP_EXEC_KUBE_CREDS and a shared volume at MAINLOOP_EXEC_KUBE_OUT, and names the namespaces
// no request may touch in MAINLOOP_EXEC_KUBE_PROTECT.
func execKubeProxy(ctx context.Context) error {
	cfg, err := config.LoadExecKubeProxy()
	if err != nil {
		return err
	}
	return privileged.ExecKubeProxy{CredDir: cfg.Creds, OutDir: cfg.Out, Protected: cfg.Protect}.Run(ctx)
}
