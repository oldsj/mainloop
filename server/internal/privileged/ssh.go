package privileged

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/redact"
)

// command.ssh runs one approved command on a home-network host over ssh, as that host's admin user
// with the executor's own key for it: the owner's terminal work on the NAS, the router, a Windows
// host or a VM that no typed action covers.
const TypeSSH = "command.ssh"

// MaxSSHCommandBytes bounds the command line the remote shell receives.
const MaxSSHCommandBytes = 8 << 10

// SSHArgs are command.ssh's arguments. Target is a host from the executor's host allowlist.
// Command is the one line the remote user's shell receives and interprets; Stdin is what the
// command reads (a script for `sh -s`, a file's new contents), shown in full on the card.
type SSHArgs struct {
	Target         string `json:"target"`
	Command        string `json:"command"`
	Stdin          string `json:"stdin,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

func (a SSHArgs) timeout() time.Duration {
	if a.TimeoutSeconds == 0 {
		return DefaultCommandTimeout
	}
	return time.Duration(a.TimeoutSeconds) * time.Second
}

func normalizeSSH(raw json.RawMessage) (any, error) {
	var a SSHArgs
	if err := decodeStrict(raw, &a); err != nil {
		return nil, err
	}
	a.Target = strings.TrimSpace(a.Target)
	switch {
	case !dnsLabelRE.MatchString(a.Target):
		return nil, invalidf("target is a host's name from the executor's host allowlist (nas, router), not an address")
	case strings.TrimSpace(a.Command) == "":
		return nil, invalidf("command is the line the remote shell runs")
	case len(a.Command) > MaxSSHCommandBytes:
		return nil, invalidf("command is at most %d KiB; put a longer script in stdin and run it with sh -s", MaxSSHCommandBytes>>10)
	case !utf8.ValidString(a.Command) || strings.ContainsFunc(a.Command, unprintable):
		return nil, invalidf("command is one line of text without control or invisible characters; put a script in stdin and run it with sh -s")
	case len(a.Stdin) > MaxStdinBytes:
		return nil, invalidf("stdin is at most %d KiB", MaxStdinBytes>>10)
	case !utf8.ValidString(a.Stdin) || strings.ContainsFunc(a.Stdin, unprintableText):
		return nil, invalidf("stdin is text: no control characters but newlines and tabs, and no invisible ones")
	case strings.Contains(strings.ReplaceAll(a.Stdin, "\r\n", ""), "\r"):
		// A bare carriage return can make a line show as something other than what runs.
		return nil, invalidf("stdin has a carriage return that does not end a line (\\r without \\n)")
	case a.TimeoutSeconds < 0 || time.Duration(a.TimeoutSeconds)*time.Second > MaxCommandTimeout:
		return nil, invalidf("timeout_seconds is 1 to %d (default %d)", int(MaxCommandTimeout.Seconds()), int(DefaultCommandTimeout.Seconds()))
	}
	// The card shows the real command, so a credential in it is refused rather than masked.
	if redact.Credential(a.Command) {
		return nil, invalidf("command looks like it holds a credential (contains_secret)")
	}
	if redact.Credential(a.Stdin) {
		return nil, invalidf("stdin holds what looks like a credential (contains_secret)")
	}
	return a, nil
}

// unprintableText is unprintable, allowing the line breaks and tabs a script has.
func unprintableText(r rune) bool {
	return r != '\n' && r != '\t' && r != '\r' && unprintable(r)
}

// sshWords are a command's words, lowercased, for the card's best-effort tags.
func sshWords(a SSHArgs) []string {
	return strings.FieldsFunc(strings.ToLower(a.Command+"\n"+a.Stdin), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-'
	})
}

// sshDeleteWords and sshPowerWords mark a command that may delete or wipe, or reboot or power off a
// host: a word scan for the card's tags and a delete's expiry, not a guard (the remote shell can
// spell anything another way).
var (
	sshDeleteWords = []string{"rm", "rmdir", "shred", "unlink", "dd", "mkfs", "wipefs", "fdisk", "sfdisk", "parted", "destroy",
		"delete", "del", "erase", "truncate", "remove-item", "format-volume", "clear-disk", "remove-partition", "drop"}
	sshPowerWords = []string{"reboot", "shutdown", "poweroff", "halt", "restart-computer", "stop-computer", "init"}
)

func sshMatches(a SSHArgs, list []string) []string {
	var out []string
	for _, w := range sshWords(a) {
		if slices.Contains(list, w) && !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

func sshExpiry(args any) time.Duration {
	if a, ok := args.(SSHArgs); ok && len(sshMatches(a, sshDeleteWords)) > 0 {
		return DeleteExpiry
	}
	return UndecidedExpiry
}

// SSHFacts are command.ssh's frozen facts: the host as the executor reached it. There is no
// preview: nothing reads what a remote shell command would do without running it.
type SSHFacts struct {
	Host    string `json:"host"`
	Address string `json:"address"`
	User    string `json:"user"`
	// HostKey is the pinned host key the host presented (SHA256 fingerprint); the executor checks
	// it again just before the command runs.
	HostKey   string    `json:"host_key"`
	CheckedAt time.Time `json:"checked_at"`
}

// sshAgentFacts are the facts the agent sees: the host by name, not its address.
func sshAgentFacts(facts json.RawMessage) json.RawMessage {
	var f SSHFacts
	if err := json.Unmarshal(facts, &f); err != nil {
		return nil
	}
	b, err := json.Marshal(struct {
		Host      string    `json:"host"`
		User      string    `json:"user"`
		HostKey   string    `json:"host_key"`
		CheckedAt time.Time `json:"checked_at"`
	}{f.Host, f.User, f.HostKey, f.CheckedAt})
	if err != nil {
		return nil
	}
	return b
}

// SSHHello is what the executor learned connecting and logging in to a host, without running
// anything.
type SSHHello struct {
	Address string
	User    string
	HostKey string
}

// SSHRun is one command on one host. Expect is the host as the owner saw it: Run starts nothing
// unless the host it reaches still matches.
type SSHRun struct {
	Host    string
	Expect  SSHHello
	Command string
	Stdin   string
	Timeout time.Duration
}

// NothingRan wraps an error from SSH.Run that came before the command started.
type NothingRan struct{ Err error }

func (e *NothingRan) Error() string { return e.Err.Error() }
func (e *NothingRan) Unwrap() error { return e.Err }

// SSH reaches the allowlisted hosts with the executor's key for each.
type SSH interface {
	// Hello connects to the host, checks its key against the pinned one, logs in, and closes:
	// nothing runs. A *refusal says why the host cannot be reached as asked.
	Hello(ctx context.Context, host string) (SSHHello, error)
	// Run connects as Hello does and runs the command once. An error before the command started
	// is a *NothingRan.
	Run(ctx context.Context, run SSHRun) (CommandOutput, error)
}

func preflightSSH(ctx context.Context, s *Service, raw json.RawMessage, now time.Time) (any, error) {
	var a SSHArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	switch {
	case s.SSH == nil:
		return nil, refuse("this executor has no ssh access")
	case !slices.Contains(s.SSHHosts, a.Target):
		return nil, refuse("target %q is not in the executor's host allowlist (%s)", a.Target, strings.Join(s.SSHHosts, ", "))
	}
	h, err := s.SSH.Hello(ctx, a.Target)
	if err != nil {
		return nil, err
	}
	return SSHFacts{Host: a.Target, Address: h.Address, User: h.User, HostKey: h.HostKey, CheckedAt: now}, nil
}

func renderSSH(r dbq.PrivilegedAction, ctx map[string]any) (string, string, map[string]any, error) {
	var f SSHFacts
	var a SSHArgs
	if json.Unmarshal(r.FrozenFacts, &f) != nil || json.Unmarshal(r.Args, &a) != nil {
		return "", "", nil, fmt.Errorf("privileged: %s facts cannot be read", r.Type)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Runs this once on %s (%s) as %s, over ssh with the executor's key for that host. "+
		"The remote user's shell interprets it, so it can do anything %s can:\n", f.Host, f.Address, f.User, f.User)
	b.WriteString(fenced("sh", a.Command))
	if a.Stdin != "" {
		b.WriteString("With this on its stdin:\n" + fenced("", a.Stdin))
	}
	fmt.Fprintf(&b, "\nNothing can preview it. The host presented its pinned key (%s); just before the command runs, "+
		"the executor checks that again, and runs nothing if it changed. ", f.HostKey)
	b.WriteString("There is no retry: if the answer is lost, the outcome is uncertain and it is not run again. ")
	b.WriteString("Its output (exit code, stdout and stderr, redacted and bounded) goes back to the agent and is stored with the action.")
	tags := []string{"remote shell", "as " + f.User + " on " + f.Host}
	if w := sshMatches(a, sshDeleteWords); len(w) > 0 {
		tags = append([]string{"destructive: may delete or wipe (" + strings.Join(w, ", ") + ")"}, tags...)
	}
	if w := sshMatches(a, sshPowerWords); len(w) > 0 {
		tags = append(tags, "may reboot or power off ("+strings.Join(w, ", ")+")")
	}
	tags = append(tags, "no preview")
	fmt.Fprintf(&b, "\nRisk: %s. (The delete and reboot tags come from a word scan, not a guarantee.)", strings.Join(tags, ", "))
	ctx["host"] = f.Host
	return fmt.Sprintf("Run a command on %s as %s over ssh", f.Host, f.User), b.String(), ctx, nil
}

// ssh runs command.ssh: the host's key again, then the command once.
func (r *Runner) ssh(ctx context.Context, row dbq.PrivilegedAction) outcome {
	var f SSHFacts
	var a SSHArgs
	if json.Unmarshal(row.FrozenFacts, &f) != nil || json.Unmarshal(row.Args, &a) != nil {
		return outcome{state: StateFailed, err: "the frozen facts cannot be read"}
	}
	if r.SSH == nil || !slices.Contains(r.SSHHosts, a.Target) {
		return outcome{state: StateFailed, err: "this executor no longer reaches " + a.Target}
	}
	if done, out := r.startedBefore(ctx, row); done {
		return out
	}
	h, err := r.SSH.Hello(ctx, a.Target)
	var refused *refusal
	switch {
	case errors.As(err, &refused):
		return outcome{state: StateStale, err: a.Target + " is not as the owner saw it: " + refused.msg + "; nothing ran"}
	case err != nil:
		return retry(err) // nothing ran yet
	case h.HostKey != f.HostKey || h.User != f.User || h.Address != f.Address:
		return outcome{state: StateStale, err: fmt.Sprintf("%s's pinned key, user or address changed since the owner saw it; nothing ran", a.Target)}
	}
	if err := r.extendLease(ctx, row, a.timeout()); err != nil {
		return retry(err)
	}
	if err := r.event(ctx, row.ID, stepStarted, boundText([]byte("ssh "+a.Target+" "+a.Command), 1<<10)); err != nil {
		return retry(err)
	}
	out, err := r.SSH.Run(ctx, SSHRun{Host: a.Target, Expect: h, Command: a.Command, Stdin: a.Stdin, Timeout: a.timeout()})
	var before *NothingRan
	if errors.As(err, &before) {
		// Final, not retried: the started event is written, and a retry must never guess.
		return outcome{state: StateFailed, err: "nothing ran on " + a.Target + ": " + safeError(before.Err)}
	}
	if err != nil {
		return outcome{state: StateUncertain, err: "the command could not be run to its end on " + a.Target + ": " + safeError(err) +
			"; it may have run, and may still be running there"}
	}
	o := commandOutcome("the command on "+a.Target, out, a.timeout())
	if out.TimedOut {
		// There is no terminal, so the host may ignore the kill signal and closing the connection.
		o.err = fmt.Sprintf("the command on %s ran past its %s; the executor asked the host to kill it and closed the connection, "+
			"but the host may ignore that, so it may still be running there, and what it changed is unknown", a.Target, a.timeout())
	}
	return o
}
