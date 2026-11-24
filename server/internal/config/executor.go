package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/caarlos0/env/v11"
)

// Executor is serve-executor's settings (docs/specs/privileged-actions.md): its own database login,
// the owner whose actions it runs, the 1Password service account token, the namespaces it may
// write Secrets in, and whether it runs the k8s.* types. It needs nothing of serve-api's, and serve-api has none of these.
type Executor struct {
	DB      Database
	OwnerID string `env:"MAINLOOP_OWNER_ID" envDefault:"local-dev-user"`
	// OnePasswordToken is a read-only 1Password Service Account token.
	OnePasswordToken Secret `env:"MAINLOOP_EXECUTOR_OP_TOKEN"`
	// SecretNamespaceList is the comma-separated namespaces onepassword.item_to_secret may write
	// Secrets in. Empty allows none.
	SecretNamespaceList string   `env:"MAINLOOP_EXECUTOR_SECRET_NAMESPACES"`
	SecretNamespaces    []string `env:"-"`
	// Namespace is the executor's own, where its narrow ServiceAccounts live; the k8s.* types never
	// change it. The chart sets it from the pod's namespace.
	Namespace string `env:"MAINLOOP_EXECUTOR_NAMESPACE"`
	// Kubernetes turns on the k8s.* types: the executor mints tokens for its narrow ServiceAccounts.
	Kubernetes ExactTrue `env:"MAINLOOP_EXECUTOR_KUBERNETES_ENABLED"`
	// Cluster names the cluster the executor acts on, for the cards.
	Cluster string `env:"MAINLOOP_EXECUTOR_CLUSTER"`
	// Omni turns on command.omnictl and command.talosctl, through Omni at OmniEndpoint with the
	// service-account key OmniKey (role Admin). talosctl may act on OmniClusters only.
	Omni            ExactTrue `env:"MAINLOOP_EXECUTOR_OMNI_ENABLED"`
	OmniEndpoint    string    `env:"MAINLOOP_EXECUTOR_OMNI_ENDPOINT"`
	OmniKey         Secret    `env:"MAINLOOP_EXECUTOR_OMNI_KEY"`
	OmniClusterList string    `env:"MAINLOOP_EXECUTOR_OMNI_CLUSTERS"`
	OmniClusters    []string  `env:"-"`
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
	e.Namespace, e.Cluster = strings.TrimSpace(e.Namespace), strings.TrimSpace(e.Cluster)
	if bool(e.Kubernetes) && !dnsLabel.MatchString(e.Namespace) {
		errs = append(errs, errors.New("MAINLOOP_EXECUTOR_NAMESPACE must be the executor's namespace when MAINLOOP_EXECUTOR_KUBERNETES_ENABLED is true"))
	}
	if e.Omni {
		e.OmniEndpoint = strings.TrimSpace(e.OmniEndpoint)
		if u, err := url.Parse(e.OmniEndpoint); err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" && u.Path != "/" {
			errs = append(errs, errors.New("MAINLOOP_EXECUTOR_OMNI_ENDPOINT must be Omni's https URL when MAINLOOP_EXECUTOR_OMNI_ENABLED is true"))
		}
		if strings.TrimSpace(e.OmniKey.Reveal()) == "" {
			errs = append(errs, errors.New("MAINLOOP_EXECUTOR_OMNI_KEY is required when MAINLOOP_EXECUTOR_OMNI_ENABLED is true"))
		}
		for _, c := range strings.Split(e.OmniClusterList, ",") {
			if c = strings.TrimSpace(c); c == "" {
				continue
			}
			if !dnsLabel.MatchString(c) {
				errs = append(errs, fmt.Errorf("MAINLOOP_EXECUTOR_OMNI_CLUSTERS: %q is not a cluster name", c))
				continue
			}
			e.OmniClusters = append(e.OmniClusters, c)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return e, nil
}
