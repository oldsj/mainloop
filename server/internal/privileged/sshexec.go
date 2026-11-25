package privileged

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

// The files of a host's credential, in Dir/<host>/: a Secret mounted from the 1Password item
// mainloop-executor-ssh-<host>.
const (
	sshKeyFile        = "private-key"
	sshUserFile       = "user"
	sshKnownHostsFile = "known-hosts"
)

// sshDialTimeout bounds connecting and logging in.
const sshDialTimeout = 15 * time.Second

// ExecSSH reaches the allowlisted hosts with an ssh client of its own (golang.org/x/crypto/ssh, no
// OpenSSH configuration): only the host's pinned key is accepted, it logs in with the executor's
// key for that host only, and it asks for nothing but the command: no agent, no forwarding, no
// terminal, no environment.
type ExecSSH struct {
	// Hosts is the host allowlist: each host's name and address.
	Hosts map[string]netip.AddrPort
	// Dir holds a directory per host with its credential's files.
	Dir string
	// Dial, if set, opens the TCP connection (tests); otherwise a plain dial.
	Dial func(ctx context.Context, addr string) (net.Conn, error)
}

// sshCredential is one host's credential, read fresh for each connection so a rotated Secret
// takes effect without a restart.
type sshCredential struct {
	user     string
	signer   ssh.Signer
	hostKeys []ssh.PublicKey
	keyText  string
}

func (e ExecSSH) credential(host string) (sshCredential, error) {
	// The host is a DNS label from the allowlist; a root keeps every read inside its directory.
	root, rootErr := os.OpenRoot(filepath.Join(e.Dir, host))
	if rootErr == nil {
		defer func() { _ = root.Close() }()
	}
	read := func(name string) (string, error) {
		var b []byte
		err := rootErr
		if err == nil {
			b, err = root.ReadFile(name)
		}
		if err != nil || strings.TrimSpace(string(b)) == "" {
			return "", refuse("credential_missing: the executor has no %s for %s (1Password item mainloop-executor-ssh-%s, field %s)", name, host, host, name)
		}
		return string(b), nil
	}
	var c sshCredential
	key, err := read(sshKeyFile)
	if err != nil {
		return c, err
	}
	user, err := read(sshUserFile)
	if err != nil {
		return c, err
	}
	known, err := read(sshKnownHostsFile)
	if err != nil {
		return c, err
	}
	c.keyText, c.user = key, strings.TrimSpace(user)
	if !sshUserRE.MatchString(c.user) {
		return c, refuse("the user for %s (item mainloop-executor-ssh-%s, field user) is not a user name", host, host)
	}
	if c.signer, err = ssh.ParsePrivateKey([]byte(key)); err != nil {
		// The parser's message can quote the key; say only which field.
		return c, refuse("the private key for %s (item mainloop-executor-ssh-%s, field private-key) cannot be read: an unencrypted OpenSSH key is needed", host, host)
	}
	if c.hostKeys, err = parseKnownHosts(known); err != nil {
		return c, refuse("the host key for %s (item mainloop-executor-ssh-%s, field known-hosts): %s", host, host, err.Error())
	}
	return c, nil
}

// sshUserRE is a POSIX or Windows local user name.
var sshUserRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9._-]{0,31}$`)

// parseKnownHosts reads the pinned host keys: known_hosts lines ("[host]:port type key", or
// "type key" alone). Markers (@revoked, @cert-authority) and hashed host names are refused: the
// pin is the key itself.
func parseKnownHosts(text string) ([]ssh.PublicKey, error) {
	var keys []ssh.PublicKey
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "@") {
			return nil, errors.New("markers such as @cert-authority are not accepted; pin the host's own key")
		}
		k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			if _, _, k2, _, _, err2 := ssh.ParseKnownHosts([]byte(line)); err2 == nil {
				k, err = k2, nil
			}
		}
		if err != nil {
			return nil, errors.New("a line is not a host key")
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil, errors.New("no host key")
	}
	return keys, nil
}

// connect dials the host, accepts only a pinned key, and logs in. The returned fingerprint is the
// key the host presented.
func (e ExecSSH) connect(ctx context.Context, name string) (*ssh.Client, SSHHello, sshCredential, error) {
	h, ok := e.Hosts[name]
	if !ok {
		return nil, SSHHello{}, sshCredential{}, refuse("target %q is not in the executor's host allowlist", name)
	}
	c, err := e.credential(name)
	if err != nil {
		return nil, SSHHello{}, c, err
	}
	addr := h.String()
	hello := SSHHello{Address: addr, User: c.user}
	var presented ssh.PublicKey
	algos := make([]string, 0, len(c.hostKeys))
	for _, k := range c.hostKeys {
		algos = append(algos, hostKeyAlgorithms(k.Type())...)
	}
	cfg := &ssh.ClientConfig{
		User: c.user,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(c.signer)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			presented = key
			for _, k := range c.hostKeys {
				if bytes.Equal(k.Marshal(), key.Marshal()) {
					return nil
				}
			}
			return errHostKey
		},
		HostKeyAlgorithms: algos,
		Timeout:           sshDialTimeout,
		ClientVersion:     "SSH-2.0-mainloop-executor",
	}
	dctx, cancel := context.WithTimeout(ctx, sshDialTimeout)
	defer cancel()
	dial := e.Dial
	if dial == nil {
		dial = func(ctx context.Context, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		}
	}
	conn, err := dial(dctx, addr)
	if err != nil {
		// The host's name only: the agent sees refusals, not the network's addresses.
		return nil, hello, c, refuse("cannot reach %s: %s", name, dialReason(err))
	}
	// The handshake has its own deadline; the dial's context does not reach it.
	_ = conn.SetDeadline(time.Now().Add(sshDialTimeout))
	cc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		switch {
		case errors.Is(err, errHostKey):
			fp := "none"
			if presented != nil {
				fp = ssh.FingerprintSHA256(presented)
			}
			return nil, hello, c, refuse("%s presented a host key that is not the pinned one (%s): something may be in the middle, or the host was reinstalled; nothing ran", name, fp)
		case strings.Contains(err.Error(), "unable to authenticate"):
			return nil, hello, c, refuse("%s refused the executor's key for user %s", name, c.user)
		}
		return nil, hello, c, fmt.Errorf("ssh to %s: handshake failed", name)
	}
	_ = conn.SetDeadline(time.Time{})
	hello.HostKey = ssh.FingerprintSHA256(presented)
	return ssh.NewClient(cc, chans, reqs), hello, c, nil
}

var errHostKey = errors.New("host key is not pinned")

// hostKeyAlgorithms are the signature algorithms a pinned key of this type may be presented with.
func hostKeyAlgorithms(keyType string) []string {
	if keyType == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}
	return []string{keyType}
}

// dialReason says why a host cannot be reached without the network's details.
func dialReason(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "no route (the executor's network may not reach it yet)"
	case errors.As(err, &ne) && ne.Timeout(), errors.Is(err, context.DeadlineExceeded):
		return "timed out (the executor's network may not reach it yet)"
	}
	return "the connection failed"
}

func (e ExecSSH) Hello(ctx context.Context, host string) (SSHHello, error) {
	client, hello, _, err := e.connect(ctx, host)
	if err != nil {
		return hello, err
	}
	_ = client.Close()
	return hello, nil
}

func (e ExecSSH) Run(ctx context.Context, run SSHRun) (CommandOutput, error) {
	client, hello, c, err := e.connect(ctx, run.Host)
	if err != nil {
		return CommandOutput{}, &NothingRan{err}
	}
	defer func() { _ = client.Close() }()
	if hello != run.Expect {
		return CommandOutput{}, &NothingRan{fmt.Errorf("%s's pinned key, user or address changed since the owner saw it", run.Host)}
	}
	creds := []string{strings.TrimSpace(c.keyText)}
	sess, err := client.NewSession()
	if err != nil {
		return CommandOutput{Credentials: creds}, &NothingRan{fmt.Errorf("ssh to %s: no session", run.Host)}
	}
	defer func() { _ = sess.Close() }()
	var stdout, stderr capped
	sess.Stdout, sess.Stderr = &stdout, &stderr
	sess.Stdin = strings.NewReader(run.Stdin)
	if err := sess.Start(run.Command); err != nil {
		// Only the host's explicit answer to the exec request (x/crypto/ssh's error when the reply
		// is a refusal) means nothing ran. Any other error came after the request was sent: the
		// host may have started the command before the connection broke.
		if err.Error() == "ssh: command "+run.Command+" failed" {
			return CommandOutput{Credentials: creds}, &NothingRan{fmt.Errorf("ssh to %s: the host refused to start the command", run.Host)}
		}
		return CommandOutput{Credentials: creds}, fmt.Errorf("ssh to %s: the connection broke after the command was sent", run.Host)
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	timer := time.NewTimer(run.Timeout)
	defer timer.Stop()
	out := func() CommandOutput {
		return CommandOutput{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Credentials: creds}
	}
	select {
	case err = <-done:
	case <-timer.C:
		_ = sess.Signal(ssh.SIGKILL)
		_ = client.Close()
		<-done
		o := out()
		o.TimedOut = true
		return o, nil
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		_ = client.Close()
		<-done
		return out(), ctx.Err()
	}
	o := out()
	var exit *ssh.ExitError
	var missing *ssh.ExitMissingError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		o.ExitCode = exit.ExitStatus()
	case errors.As(err, &missing):
		return o, errors.New("the host closed the session without an exit status")
	default:
		return o, fmt.Errorf("ssh to %s: the session ended early", run.Host)
	}
	return o, nil
}
