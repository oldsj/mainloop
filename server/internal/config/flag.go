package config

// ExactTrue is a deployment gate that is on only for the exact text "true". Any other value
// (including "TRUE", "1", "t" and the empty string) leaves it off and does not fail startup, so a
// mistyped opt-in fails closed.
type ExactTrue bool

// UnmarshalText implements [encoding.TextUnmarshaler].
func (f *ExactTrue) UnmarshalText(text []byte) error {
	*f = string(text) == "true"
	return nil
}
