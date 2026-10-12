package config

import (
	"errors"
	"fmt"

	"github.com/caarlos0/env/v11"
)

// LoadKagent reads only the kagent settings from the process environment, for commands that
// talk to kagent and need nothing else, such as kagent-probe.
func LoadKagent() (*Kagent, error) {
	return ParseKagent(nil)
}

// ParseKagent reads and validates the kagent settings from the given variables. A nil map
// means the process environment.
func ParseKagent(vars map[string]string) (*Kagent, error) {
	k := &Kagent{}
	opts := env.Options{Environment: vars}
	if vars == nil {
		opts.Environment = environ()
	}
	if err := env.ParseWithOptions(k, opts); err != nil {
		return nil, fmt.Errorf("config: %w", nameParseErrors(err, opts.Environment))
	}
	if err := errors.Join(k.validate()...); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return k, nil
}

// validate checks the kagent settings that hold without the rest of the configuration.
func (k *Kagent) validate() []error {
	var errs []error
	if k.ControlTokenFile != "" {
		if k.UserID != "mainloop" {
			errs = append(errs, errors.New("service-token mode requires KAGENT_USER_ID=mainloop"))
		}
		if err := checkControlTokenFile(k.ControlTokenFile); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}
