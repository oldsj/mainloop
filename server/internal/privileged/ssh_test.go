package privileged_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/privileged"
)

// sshKeyFixture stands for the host's private key the executor holds: the result must never carry
// it.
const sshKeyFixture = "-----BEGIN OPENSSH PRIVATE KEY-----\nZmljdGlvbmFsLWtleS1mb3ItdGVzdHM=\n-----END OPENSSH PRIVATE KEY-----"

// fakeSSH is one allowlisted host, nas-01. hello is what it answers; runs keeps every command.
type fakeSSH struct {
	hello    privileged.SSHHello
	helloErr error
	hellos   int
	runs     []privileged.SSHRun
	handle   func(run privileged.SSHRun) (privileged.CommandOutput, error)
}

func (f *fakeSSH) Hello(_ context.Context, host string) (privileged.SSHHello, error) {
	f.hellos++
	if host != sshHost {
		return privileged.SSHHello{}, fmt.Errorf("no host %s", host)
	}
	return f.hello, f.helloErr
}

func (f *fakeSSH) Run(_ context.Context, run privileged.SSHRun) (privileged.CommandOutput, error) {
	f.runs = append(f.runs, run)
	out, err := f.handle(run)
	out.Credentials = []string{sshKeyFixture}
	return out, err
}

const sshHost = "nas-01"

var nasHello = privileged.SSHHello{Address: "192.0.2.20:22", User: "admin", HostKey: "SHA256:ZmljdGlvbmFsLWhvc3Qta2V5LWZpbmdlcnByaW50"}

func newSSHEnv(t *testing.T, handle func(run privileged.SSHRun) (privileged.CommandOutput, error)) (*kubeEnv, *fakeSSH) {
	t.Helper()
	e := newKubeEnv(t)
	e.svc.SSHOn = true
	f := &fakeSSH{hello: nasHello, handle: handle}
	e.ex.SSH, e.ex.SSHHosts = f, []string{sshHost, "router"}
	return e, f
}

func sshArgs(command string) map[string]any {
	return map[string]any{"target": sshHost, "command": command}
}

func printed(stdout string) func(privileged.SSHRun) (privileged.CommandOutput, error) {
	return func(privileged.SSHRun) (privileged.CommandOutput, error) {
		return privileged.CommandOutput{Stdout: []byte(stdout)}, nil
	}
}

// Given an allowlisted host, when main requests a command on it, then preflight only connects and
// checks the pinned key, and the card shows the host, its address, the user, the key and the
// exact command and stdin. After an Approve the executor checks the host again, runs the command
// once, and returns its output without the key it held.
func TestSSHChecksTheHostAndRunsOnce(t *testing.T) {
	e, f := newSSHEnv(t, printed("Filesystem  Size  Used\n/dev/md0  7.3T  2.1T\nkey "+sshKeyFixture+"\n"))
	args := sshArgs("sh -s")
	args["stdin"] = "df -h /volume1\nzpool status\n"
	a := e.request(t, privileged.TypeSSH, "ssh-1", args)
	a, card := e.preflighted(t, a.ID)
	cardHas(t, card, "Run a command on nas-01 as admin over ssh", "on nas-01 (192.0.2.20:22) as admin", "sh -s",
		"With this on its stdin", "zpool status", nasHello.HostKey, "Nothing can preview it",
		"Risk: remote shell, as admin on nas-01, no preview.",
		"Its output (exit code, stdout and stderr, redacted and bounded) goes back to the agent and is stored with the action.")
	if seen := string(privileged.AgentFacts(a.Type, a.Facts)); strings.Contains(seen, "192.0.2.20") || !strings.Contains(seen, nasHello.HostKey) {
		t.Fatalf("the agent sees %s", seen)
	}
	if f.hellos != 1 || len(f.runs) != 0 {
		t.Fatalf("preflight: %d hellos, runs %+v", f.hellos, f.runs)
	}
	e.approve(t, a.ID)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded {
		t.Fatalf("after execution: %+v (%v)", a, deref(a.LastError))
	}
	if f.hellos != 2 || len(f.runs) != 1 || f.runs[0].Command != "sh -s" || f.runs[0].Stdin != args["stdin"] ||
		f.runs[0].Expect != nasHello || f.runs[0].Timeout != privileged.DefaultCommandTimeout {
		t.Fatalf("runs = %+v", f.runs)
	}
	res := kubectlResult(t, a)
	if !strings.Contains(res.Stdout, "/dev/md0") || strings.Contains(res.Stdout, "ZmljdGlvbmFsLWtleS1mb3ItdGVzdHM=") {
		t.Fatalf("result = %+v", res)
	}
}

// A command that exits non-zero fails with its output; one that ran past its timeout, or whose
// connection broke after it was sent, is uncertain and may still be running.
func TestSSHExitAndTimeout(t *testing.T) {
	for name, tc := range map[string]struct {
		out  privileged.CommandOutput
		err  error
		want string
		why  string
	}{
		"exit":    {privileged.CommandOutput{ExitCode: 3, Stderr: []byte("no such pool\n")}, nil, privileged.StateFailed, "exit"},
		"timeout": {privileged.CommandOutput{TimedOut: true}, nil, privileged.StateUncertain, "may still be running there"},
		"broken": {privileged.CommandOutput{}, errors.New("ssh to nas-01: the connection broke after the command was sent"),
			privileged.StateUncertain, "may still be running there"},
	} {
		t.Run(name, func(t *testing.T) {
			e, f := newSSHEnv(t, func(privileged.SSHRun) (privileged.CommandOutput, error) { return tc.out, tc.err })
			a := e.request(t, privileged.TypeSSH, "ssh-1", sshArgs("zpool status tank"))
			a, _ = e.preflighted(t, a.ID)
			e.approve(t, a.ID)
			e.runExecutor(t)
			if a = e.get(t, a.ID); a.State != tc.want || !strings.Contains(deref(a.LastError), tc.why) || len(f.runs) != 1 {
				t.Fatalf("action = %+v (%v), runs %d", a, deref(a.LastError), len(f.runs))
			}
		})
	}
}

// Between the card and the run, a host whose key, user or address changed, or that now refuses
// the executor, runs nothing; an earlier attempt that may have started the command leaves the
// action uncertain; an error before the command started fails it, not retried.
func TestSSHRunsNothingWhenTheHostChanged(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(f *fakeSSH, e *kubeEnv, id string)
		want   string
		why    string
	}{
		"host key": {func(f *fakeSSH, _ *kubeEnv, _ string) { f.hello.HostKey = "SHA256:b3RoZXIta2V5" }, privileged.StateStale, "pinned key, user or address changed"},
		"user":     {func(f *fakeSSH, _ *kubeEnv, _ string) { f.hello.User = "root" }, privileged.StateStale, "pinned key, user or address changed"},
		"refused": {func(f *fakeSSH, _ *kubeEnv, _ string) {
			f.helloErr = privileged.Refusal("nas-01 presented a host key that is not the pinned one")
		}, privileged.StateStale, "is not as the owner saw it"},
		"started before": {func(_ *fakeSSH, e *kubeEnv, id string) {
			e.exec(t, `INSERT INTO privileged_action_events (action_id, step, detail) VALUES ($1, 'command_started', 'ssh nas-01')`, id)
		}, privileged.StateUncertain, ""},
	} {
		t.Run(name, func(t *testing.T) {
			e, f := newSSHEnv(t, printed("ok\n"))
			a := e.request(t, privileged.TypeSSH, "ssh-1", sshArgs("reboot"))
			a, _ = e.preflighted(t, a.ID)
			e.approve(t, a.ID)
			tc.change(f, e, a.ID)
			e.runExecutor(t)
			if a = e.get(t, a.ID); a.State != tc.want || !strings.Contains(deref(a.LastError), tc.why) || len(f.runs) != 0 {
				t.Fatalf("action = %+v (%v), runs %+v", a, deref(a.LastError), f.runs)
			}
		})
	}

	e, f := newSSHEnv(t, func(privileged.SSHRun) (privileged.CommandOutput, error) {
		return privileged.CommandOutput{}, &privileged.NothingRan{Err: errors.New("the host refused to start the command")}
	})
	a := e.request(t, privileged.TypeSSH, "ssh-2", sshArgs("uptime"))
	a, _ = e.preflighted(t, a.ID)
	e.approve(t, a.ID)
	e.runExecutor(t)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateFailed || !strings.Contains(deref(a.LastError), "nothing ran on nas-01") || len(f.runs) != 1 {
		t.Fatalf("action = %+v (%v), runs %d", a, deref(a.LastError), len(f.runs))
	}
}

// A command naming a delete or a reboot is tagged so (a word scan); one that may delete expires in
// an hour.
func TestSSHTagsAndExpiry(t *testing.T) {
	for _, tc := range []struct {
		command string
		tags    []string
		expiry  time.Duration
	}{
		{"rm -rf /volume1/tmp/old-backups", []string{"Risk: destructive: may delete or wipe (rm)"}, privileged.DeleteExpiry},
		{"Remove-Item C:\\Temp\\old -Recurse", []string{"may delete or wipe (remove-item)"}, privileged.DeleteExpiry},
		{"sudo reboot", []string{"may reboot or power off (reboot)"}, privileged.UndecidedExpiry},
		{"uptime", []string{"Risk: remote shell, as admin on nas-01, no preview."}, privileged.UndecidedExpiry},
	} {
		e, _ := newSSHEnv(t, printed(""))
		a := e.request(t, privileged.TypeSSH, "x1", sshArgs(tc.command))
		if got := a.ExpiresAt.Sub(a.CreatedAt).Round(time.Minute); got != tc.expiry {
			t.Errorf("%s: expires after %v, want %v", tc.command, got, tc.expiry)
		}
		_, card := e.preflighted(t, a.ID)
		cardHas(t, card, tc.tags...)
	}
}

// Requests the executor never runs are refused before anything connects.
func TestSSHRequestRefusals(t *testing.T) {
	e, f := newSSHEnv(t, printed(""))
	for i, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"target": "192.0.2.20", "command": "uptime"}, "not an address"},
		{map[string]any{"target": "nas-01", "command": " "}, "command is the line the remote shell runs"},
		{map[string]any{"target": "nas-01", "command": "uptime\nreboot"}, "one line of text"},
		{map[string]any{"target": "nas-01", "command": "echo \u202eok"}, "one line of text"},
		{map[string]any{"target": "nas-01", "command": strings.Repeat("x", privileged.MaxSSHCommandBytes+1)}, "at most 8 KiB"},
		{map[string]any{"target": "nas-01", "command": "sh -s", "stdin": "echo \x1b[2J"}, "stdin is text"},
		{map[string]any{"target": "nas-01", "command": "sh -s", "stdin": "echo ok\rrm -rf /tmp/x\n"}, "carriage return that does not end a line"},
		{map[string]any{"target": "nas-01", "command": "uptime", "timeout_seconds": 3600}, "timeout_seconds is 1 to"},
		{map[string]any{"target": "nas-01", "command": "mysql --password=hunter2hunter2"}, "looks like it holds a credential"},
		{map[string]any{"target": "nas-01", "command": "sh -s", "stdin": "export AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY\n"}, "stdin holds what looks like a credential"},
		{map[string]any{"target": "nas-01", "command": "uptime", "user": "root"}, "unknown field"},
	} {
		_, err := e.svc.Request(context.Background(), mainSession, privileged.TypeSSH, jsonArgs(tc.args), "r", fmt.Sprintf("r%d", i))
		if !errors.Is(err, privileged.ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.args, err, tc.want)
		}
	}
	if f.hellos != 0 || len(f.runs) != 0 {
		t.Fatalf("hellos %d, runs %+v", f.hellos, f.runs)
	}
}

// A host off the allowlist, or one the executor cannot reach as pinned, is refused at preflight
// with the reason; nothing runs and no card opens.
func TestSSHPreflightRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		target   string
		helloErr error
		want     string
	}{
		"not allowlisted": {"backup-02", nil, `target "backup-02" is not in the executor's host allowlist (nas-01, router)`},
		"host key":        {sshHost, privileged.Refusal("nas-01 presented a host key that is not the pinned one (SHA256:b3RoZXI)"), "not the pinned one"},
		"no credential": {sshHost, privileged.Refusal("credential_missing: the executor has no private-key for nas-01 (1Password item mainloop-executor-ssh-nas-01, field private-key)"),
			"item mainloop-executor-ssh-nas-01, field private-key"},
	} {
		t.Run(name, func(t *testing.T) {
			e, f := newSSHEnv(t, printed(""))
			f.helloErr = tc.helloErr
			a := e.request(t, privileged.TypeSSH, "r1", map[string]any{"target": tc.target, "command": "uptime"})
			e.runExecutor(t)
			e.run(t)
			if a = e.get(t, a.ID); a.State != privileged.StatePreflightFailed || !strings.Contains(deref(a.LastError), tc.want) {
				t.Fatalf("action = %+v (%v), want %q", a, deref(a.LastError), tc.want)
			}
			if len(f.runs) != 0 || len(e.openItems(t)) != 0 {
				t.Fatalf("runs = %+v", f.runs)
			}
		})
	}
}

// Without ssh access, serve-api answers command.ssh unavailable.
func TestSSHNeedsSSHAccess(t *testing.T) {
	e, _ := newSSHEnv(t, printed(""))
	e.svc.SSHOn = false
	_, err := e.svc.Request(context.Background(), mainSession, privileged.TypeSSH, jsonArgs(sshArgs("uptime")), "r", "r1")
	if !errors.Is(err, privileged.ErrUnsupported) || !strings.Contains(err.Error(), "ssh access") {
		t.Fatalf("err = %v", err)
	}
}
