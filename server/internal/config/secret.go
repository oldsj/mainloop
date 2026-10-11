package config

import "log/slog"

const redacted = "[redacted]"

// Secret holds a sensitive value. Every formatting, logging and marshalling path prints a
// placeholder; call Reveal at the one place that needs the value.
type Secret string

// Reveal returns the secret value.
func (s Secret) Reveal() string { return string(s) }

// Set reports whether the secret is non-empty.
func (s Secret) Set() bool { return s != "" }

func (Secret) String() string               { return redacted }
func (Secret) GoString() string             { return redacted }
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }
func (Secret) LogValue() slog.Value         { return slog.StringValue(redacted) }

// UnmarshalText lets caarlos0/env parse the raw value into a Secret.
func (s *Secret) UnmarshalText(text []byte) error {
	*s = Secret(text)
	return nil
}
