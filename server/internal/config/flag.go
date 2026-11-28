package config

import "fmt"

// ExactTrue is a deployment gate that is on only for the exact text "true". Any other value
// (including "TRUE", "1", "t" and the empty string) leaves it off and does not fail startup, so a
// mistyped opt-in fails closed.
type ExactTrue bool

// UnmarshalText implements [encoding.TextUnmarshaler].
func (f *ExactTrue) UnmarshalText(text []byte) error {
	*f = string(text) == "true"
	return nil
}

// ApprovalsMode says how the executor's action types are approved
// (docs/specs/privileged-actions.md, "Approval channels"). It is chosen explicitly: empty means
// unset, which the processes refuse to start with when an executor is deployed.
type ApprovalsMode string

// The two approval modes. ApprovalsWeb runs an approval the web app recorded, with no signature
// to check. ApprovalsSigned runs only an approval a pinned phone signed with Touch ID.
const (
	ApprovalsUnset  ApprovalsMode = ""
	ApprovalsWeb    ApprovalsMode = "web"
	ApprovalsSigned ApprovalsMode = "signed"
)

// UnmarshalText implements [encoding.TextUnmarshaler]. Only the exact texts "web" and "signed"
// (and unset) are accepted, so a mistyped mode stops the process instead of choosing one.
func (m *ApprovalsMode) UnmarshalText(text []byte) error {
	switch v := ApprovalsMode(text); v {
	case ApprovalsUnset, ApprovalsWeb, ApprovalsSigned:
		*m = v
		return nil
	}
	return fmt.Errorf("MAINLOOP_PRIVILEGED_APPROVALS_MODE must be %q or %q, not %q", ApprovalsWeb, ApprovalsSigned, text)
}
