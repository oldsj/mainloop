package main

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/oldsj/mainloop/server/internal/privileged"
)

// execKubeProxy runs the sidecar of a command.exec run with cluster reach
// (docs/specs/privileged-actions.md): the executor mounts the run's credential at
// MAINLOOP_EXEC_KUBE_CREDS and a shared volume at MAINLOOP_EXEC_KUBE_OUT, and names the namespaces
// no request may touch in MAINLOOP_EXEC_KUBE_PROTECT.
func execKubeProxy(ctx context.Context) error {
	var protect []string
	for _, ns := range strings.Split(os.Getenv("MAINLOOP_EXEC_KUBE_PROTECT"), ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			protect = append(protect, ns)
		}
	}
	if len(protect) == 0 {
		return errors.New("MAINLOOP_EXEC_KUBE_PROTECT must name the executor's and the toolbox's namespaces")
	}
	creds, out := os.Getenv("MAINLOOP_EXEC_KUBE_CREDS"), os.Getenv("MAINLOOP_EXEC_KUBE_OUT")
	if creds == "" || out == "" {
		return errors.New("MAINLOOP_EXEC_KUBE_CREDS and MAINLOOP_EXEC_KUBE_OUT are required")
	}
	return privileged.ExecKubeProxy{CredDir: creds, OutDir: out, Protected: protect}.Run(ctx)
}
