package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/caarlos0/env/v11"
)

// Executor is serve-executor's settings (docs/specs/privileged-actions.md): its own database login,
// the owner whose actions it runs, the 1Password service account token and the namespaces it may
// write Secrets in. It needs nothing of serve-api's, and serve-api has none of these.
type Executor struct {
	DB      Database
	OwnerID string `env:"MAINLOOP_OWNER_ID" envDefault:"local-dev-user"`
	// OnePasswordToken is a read-only 1Password Service Account token.
	OnePasswordToken Secret `env:"MAINLOOP_EXECUTOR_OP_TOKEN"`
	// SecretNamespaceList is the comma-separated namespaces onepassword.item_to_secret may write
	// Secrets in. Empty allows none.
	SecretNamespaceList string   `env:"MAINLOOP_EXECUTOR_SECRET_NAMESPACES"`
	SecretNamespaces    []string `env:"-"`
}

// LoadExecutor reads and validates serve-executor's settings from the process environment.
func LoadExecutor() (*Executor, error) {
	return ParseExecutor(nil)
}

// ParseExecutor reads the given variables; a nil map means the process environment.
func ParseExecutor(vars map[string]string) (*Executor, error) {
	e := &Executor{}
	opts := env.Options{Environment: vars}
	if vars == nil {
		opts.Environment = environ()
	}
	if err := env.ParseWithOptions(e, opts); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var errs []error
	if e.OwnerID = strings.TrimSpace(e.OwnerID); e.OwnerID == "" {
		errs = append(errs, errors.New("MAINLOOP_OWNER_ID must not be blank"))
	}
	if strings.TrimSpace(e.OnePasswordToken.Reveal()) == "" {
		errs = append(errs, errors.New("MAINLOOP_EXECUTOR_OP_TOKEN is required"))
	}
	for _, ns := range strings.Split(e.SecretNamespaceList, ",") {
		if ns = strings.TrimSpace(ns); ns == "" {
			continue
		}
		if !dnsLabel.MatchString(ns) {
			errs = append(errs, fmt.Errorf("MAINLOOP_EXECUTOR_SECRET_NAMESPACES: %q is not a namespace name", ns))
			continue
		}
		e.SecretNamespaces = append(e.SecretNamespaces, ns)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return e, nil
}
