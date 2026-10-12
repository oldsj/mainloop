package config

import (
	"fmt"

	"github.com/caarlos0/env/v11"
)

// Migrate is the subset of settings the migrate command needs: the database connection and the
// switch that allows destroying it. It does not require the agent token key.
type Migrate struct {
	DB Database
	// AllowReset permits `migrate reset`, which drops the whole schema. Only the dev-live overlay
	// sets it, and only the exact value 1 counts.
	AllowReset bool `env:"-"`
	// ResetFlag is the raw MAINLOOP_ALLOW_RESET value; AllowReset is derived from it.
	ResetFlag string `env:"MAINLOOP_ALLOW_RESET"`
}

// LoadMigrate reads the migrate settings from the process environment.
func LoadMigrate() (*Migrate, error) {
	m := &Migrate{}
	if err := env.ParseWithOptions(m, env.Options{Environment: environ()}); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	m.AllowReset = m.ResetFlag == "1"
	return m, nil
}
