package privileged

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshTestHost is an ssh server on loopback: it accepts one client key, runs no shell, and answers
// each exec from handle. It records the commands it was asked to run.
type sshTestHost struct {
	addr     netip.AddrPort
	hostKey  ssh.Signer
	mu       sync.Mutex
	commands []string
	handle   func(command, stdin string, signal <-chan struct{}) (stdout, stderr string, exit int, ok bool)
}

func newKey(t *testing.T) (ssh.Signer, []byte) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	return signer, pem.EncodeToMemory(block)
}

func startSSHHost(t *testing.T, client ssh.PublicKey) *sshTestHost {
	t.Helper()
	h := &sshTestHost{handle: func(command, stdin string, _ <-chan struct{}) (string, string, int, bool) {
		return "ran: " + command + "\nstdin: " + stdin, "", 0, true
	}}
	h.hostKey, _ = newKey(t)
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if bytes.Equal(key.Marshal(), client.Marshal()) {
			return nil, nil
		}
		return nil, errors.New("unknown key")
	}}
	cfg.AddHostKey(h.hostKey)
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	h.addr = netip.MustParseAddrPort(ln.Addr().String())
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go h.serve(conn, cfg)
		}
	}()
	return h
}

func (h *sshTestHost) serve(conn net.Conn, cfg *ssh.ServerConfig) {
	defer func() { _ = conn.Close() }()
	_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "no")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			return
		}
		go h.session(ch, creqs)
	}
}

func (h *sshTestHost) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer func() { _ = ch.Close() }()
	signal := make(chan struct{}, 1)
	for req := range reqs {
		switch req.Type {
		case "signal":
			signal <- struct{}{}
			_ = req.Reply(false, nil)
		case "exec":
			if len(req.Payload) < 4 {
				_ = req.Reply(false, nil)
				continue
			}
			command := string(req.Payload[4 : 4+binary.BigEndian.Uint32(req.Payload)])
			h.mu.Lock()
			h.commands = append(h.commands, command)
			h.mu.Unlock()
			if command == "refuse" {
				_ = req.Reply(false, nil)
				continue
			}
			_ = req.Reply(true, nil)
			go func() {
				stdin, _ := io.ReadAll(ch)
				h.mu.Lock()
				handle := h.handle
				h.mu.Unlock()
				stdout, stderr, exit, ok := handle(command, string(stdin), signal)
				_, _ = io.WriteString(ch, stdout)
				_, _ = io.WriteString(ch.Stderr(), stderr)
				if ok {
					_, _ = ch.SendRequest("exit-status", false, binary.BigEndian.AppendUint32(nil, uint32(exit)))
				}
				_ = ch.Close()
			}()
		default:
			_ = req.Reply(false, nil)
		}
	}
}

func (h *sshTestHost) setHandle(f func(command, stdin string, signal <-chan struct{}) (string, string, int, bool)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handle = f
}

func (h *sshTestHost) ran() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.commands...)
}

// sshFixture is an executor holding a credential for host nas-01, and the host itself.
func sshFixture(t *testing.T) (ExecSSH, *sshTestHost, string) {
	t.Helper()
	client, keyPEM := newKey(t)
	host := startSSHHost(t, client.PublicKey())
	dir := t.TempDir()
	writeCredential(t, dir, "nas-01", map[string]string{
		sshKeyFile: string(keyPEM), sshUserFile: "admin\n",
		sshKnownHostsFile: "# nas-01\n" + string(ssh.MarshalAuthorizedKey(host.hostKey.PublicKey())),
	})
	return ExecSSH{Hosts: map[string]netip.AddrPort{"nas-01": host.addr}, Dir: dir}, host, string(keyPEM)
}

func writeCredential(t *testing.T, dir, host string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, host), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, v := range files {
		if err := os.WriteFile(filepath.Join(dir, host, name), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Given a host presenting its pinned key, Hello logs in and runs nothing; Run runs the command
// once with its stdin and returns its output, exit code, and the key as a credential to clean.
func TestExecSSHRunsOnThePinnedHost(t *testing.T) {
	e, host, keyPEM := sshFixture(t)
	ctx := context.Background()
	hello, err := e.Hello(ctx, "nas-01")
	want := SSHHello{Address: host.addr.String(), User: "admin", HostKey: ssh.FingerprintSHA256(host.hostKey.PublicKey())}
	if err != nil || hello != want || len(host.ran()) != 0 {
		t.Fatalf("hello = %+v, %v; ran %v", hello, err, host.ran())
	}
	out, err := e.Run(ctx, SSHRun{Host: "nas-01", Expect: hello, Command: "sh -s", Stdin: "zpool status\n", Timeout: 10 * time.Second})
	if err != nil || string(out.Stdout) != "ran: sh -s\nstdin: zpool status\n" || out.ExitCode != 0 || out.TimedOut ||
		len(out.Credentials) != 1 || out.Credentials[0] != strings.TrimSpace(keyPEM) {
		t.Fatalf("out = %+v, %v", out, err)
	}
	host.setHandle(func(string, string, <-chan struct{}) (string, string, int, bool) { return "", "no such pool\n", 3, true })
	out, err = e.Run(ctx, SSHRun{Host: "nas-01", Expect: hello, Command: "zpool status tank", Timeout: 10 * time.Second})
	if err != nil || out.ExitCode != 3 || string(out.Stderr) != "no such pool\n" {
		t.Fatalf("out = %+v, %v", out, err)
	}
	if len(host.ran()) != 2 {
		t.Fatalf("ran %v", host.ran())
	}
}

// A host's known-hosts line may name it ("[address]:port type key"), as ssh-keyscan prints it.
func TestExecSSHKnownHostsWithAHostName(t *testing.T) {
	e, host, _ := sshFixture(t)
	line := "[192.0.2.20]:22 " + string(ssh.MarshalAuthorizedKey(host.hostKey.PublicKey()))
	writeCredential(t, e.Dir, "nas-01", map[string]string{sshKnownHostsFile: line})
	if _, err := e.Hello(context.Background(), "nas-01"); err != nil {
		t.Fatal(err)
	}
}

// Every way the host is not as pinned, or the credential is not usable, is refused before a
// command is sent, and says which item and field to fix without quoting it.
func TestExecSSHRefusals(t *testing.T) {
	other, _ := newKey(t)
	_, otherPEM := newKey(t)
	for name, tc := range map[string]struct {
		host  string
		files map[string]string
		want  string
	}{
		"not allowlisted":  {"backup-02", nil, `target "backup-02" is not in the executor's host allowlist`},
		"another host key": {"nas-01", map[string]string{sshKnownHostsFile: string(ssh.MarshalAuthorizedKey(other.PublicKey()))}, "presented a host key that is not the pinned one (SHA256:"},
		"key not accepted": {"nas-01", map[string]string{sshKeyFile: string(otherPEM)}, "nas-01 refused the executor's key for user admin"},
		"no known hosts":   {"nas-01", map[string]string{sshKnownHostsFile: "\n"}, "credential_missing: the executor has no known-hosts for nas-01 (1Password item mainloop-executor-ssh-nas-01, field known-hosts)"},
		"cert authority":   {"nas-01", map[string]string{sshKnownHostsFile: "@cert-authority * " + string(ssh.MarshalAuthorizedKey(other.PublicKey()))}, "markers such as @cert-authority are not accepted"},
		"not a key":        {"nas-01", map[string]string{sshKeyFile: "not-a-key-but-a-secret-value"}, "field private-key) cannot be read"},
		"bad user":         {"nas-01", map[string]string{sshUserFile: "admin; reboot"}, "field user) is not a user name"},
	} {
		t.Run(name, func(t *testing.T) {
			e, host, _ := sshFixture(t)
			if tc.files != nil {
				writeCredential(t, e.Dir, "nas-01", tc.files)
			}
			_, err := e.Hello(context.Background(), tc.host)
			var r *refusal
			if !errors.As(err, &r) || !strings.Contains(r.msg, tc.want) || strings.Contains(r.msg, "secret-value") {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			_, err = e.Run(context.Background(), SSHRun{Host: tc.host, Command: "uptime", Timeout: time.Second})
			var nothing *NothingRan
			if !errors.As(err, &nothing) || len(host.ran()) != 0 {
				t.Fatalf("Run: %v, ran %v", err, host.ran())
			}
		})
	}
}

// Run starts nothing on a host that is not as the owner saw it, or that refuses the exec.
func TestExecSSHRunStartsNothing(t *testing.T) {
	e, host, _ := sshFixture(t)
	ctx := context.Background()
	hello, err := e.Hello(ctx, "nas-01")
	if err != nil {
		t.Fatal(err)
	}
	moved := hello
	moved.HostKey = "SHA256:b3RoZXI"
	var nothing *NothingRan
	if _, err := e.Run(ctx, SSHRun{Host: "nas-01", Expect: moved, Command: "uptime", Timeout: time.Second}); !errors.As(err, &nothing) || len(host.ran()) != 0 {
		t.Fatalf("Run: %v, ran %v", err, host.ran())
	}
	if _, err := e.Run(ctx, SSHRun{Host: "nas-01", Expect: hello, Command: "refuse", Timeout: time.Second}); !errors.As(err, &nothing) {
		t.Fatalf("Run: %v", err)
	}
}

// A command that runs past its timeout is killed, and the output so far returned as timed out; a
// session that ends without an exit status is an error, not a success.
func TestExecSSHTimeoutAndNoExitStatus(t *testing.T) {
	e, host, _ := sshFixture(t)
	ctx := context.Background()
	hello, err := e.Hello(ctx, "nas-01")
	if err != nil {
		t.Fatal(err)
	}
	host.setHandle(func(_, _ string, signal <-chan struct{}) (string, string, int, bool) {
		select {
		case <-signal:
		case <-time.After(10 * time.Second):
		}
		return "partial\n", "", 0, false
	})
	start := time.Now()
	out, err := e.Run(ctx, SSHRun{Host: "nas-01", Expect: hello, Command: "sleep 600", Timeout: 200 * time.Millisecond})
	if err != nil || !out.TimedOut || time.Since(start) > 5*time.Second {
		t.Fatalf("out = %+v, %v after %v", out, err, time.Since(start))
	}
	host.setHandle(func(string, string, <-chan struct{}) (string, string, int, bool) { return "bye\n", "", 0, false })
	if _, err := e.Run(ctx, SSHRun{Host: "nas-01", Expect: hello, Command: "exit", Timeout: 5 * time.Second}); err == nil ||
		!strings.Contains(err.Error(), "without an exit status") {
		t.Fatalf("err = %v", err)
	}
}

// A host the executor cannot reach says why without the network's details.
func TestExecSSHUnreachable(t *testing.T) {
	e, _, _ := sshFixture(t)
	for errno, want := range map[syscall.Errno]string{
		syscall.ECONNREFUSED: "connection refused",
		syscall.EHOSTUNREACH: "no route (the executor's network may not reach it yet)",
	} {
		e.Dial = func(context.Context, string) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errno)}
		}
		_, err := e.Hello(context.Background(), "nas-01")
		var r *refusal
		if !errors.As(err, &r) || !strings.Contains(r.msg, "cannot reach nas-01") || !strings.Contains(r.msg, want) {
			t.Errorf("%v: err = %v", errno, err)
		}
	}
}
