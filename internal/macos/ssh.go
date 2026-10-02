package macos

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshTarget is how to reach a guest's SSH server.
type sshTarget struct {
	Addr     string
	User     string
	Password string
	// HostKey is the guest key seen first (base64). Empty trusts and
	// records the next one through OnNewKey.
	HostKey  string
	OnNewKey func(key string)
}

// sshDial connects with password or keyboard-interactive auth (macOS sshd
// offers the latter for passwords). The guest lives on the Mac's private
// vmnet bridge, so its key is trusted on first use and pinned after that.
func sshDial(ctx context.Context, t sshTarget) (*ssh.Client, error) {
	pw := t.Password
	cfg := &ssh.ClientConfig{
		User: t.User,
		Auth: []ssh.AuthMethod{
			ssh.Password(pw),
			ssh.KeyboardInteractive(func(name, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range questions {
					answers[i] = pw
				}
				return answers, nil
			}),
		},
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			got := base64.StdEncoding.EncodeToString(key.Marshal())
			if t.HostKey == "" {
				if t.OnNewKey != nil {
					t.OnNewKey(got)
				}
				return nil
			}
			if got != t.HostKey {
				return errors.New("guest SSH host key changed; delete and recreate the sandbox if this is expected")
			}
			return nil
		},
		Timeout: 15 * time.Second,
	}
	d := net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", t.Addr)
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, t.Addr, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return ssh.NewClient(c, chans, reqs), nil
}

// sshExec runs one command and returns combined output.
func sshExec(ctx context.Context, t sshTarget, cmd string) (string, error) {
	c, err := sshDial(ctx, t)
	if err != nil {
		return "", err
	}
	defer c.Close()
	s, err := c.NewSession()
	if err != nil {
		return "", err
	}
	defer s.Close()
	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	// CombinedOutput serialises stdout and stderr into one buffer safely.
	go func() {
		out, err := s.CombinedOutput(cmd)
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return string(r.out), fmt.Errorf("%w: %s", r.err, strings.TrimSpace(string(r.out)))
		}
		return string(r.out), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// waitSSH retries until the guest accepts a login or ctx ends.
func waitSSH(ctx context.Context, t sshTarget) error {
	var last error
	for {
		c, err := sshDial(ctx, t)
		if err == nil {
			c.Close()
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("guest SSH did not come up: %w", last)
		case <-time.After(3 * time.Second):
		}
	}
}

// sshSession is an interactive shell over SSH.
type sshSession struct {
	client *ssh.Client
	sess   *ssh.Session
	stdin  io.WriteCloser
	stdout io.Reader
}

func sshShell(ctx context.Context, t sshTarget, cols, rows int) (Session, error) {
	c, err := sshDial(ctx, t)
	if err != nil {
		return nil, err
	}
	s, err := c.NewSession()
	if err != nil {
		c.Close()
		return nil, err
	}
	in, err := s.StdinPipe()
	if err != nil {
		c.Close()
		return nil, err
	}
	out, err := s.StdoutPipe()
	if err != nil {
		c.Close()
		return nil, err
	}
	s.Stderr = nil // merged by the pty
	if cols <= 0 || rows <= 0 {
		cols, rows = 120, 32
	}
	if err := s.RequestPty("xterm-256color", rows, cols, ssh.TerminalModes{ssh.ECHO: 1}); err != nil {
		c.Close()
		return nil, err
	}
	if err := s.Shell(); err != nil {
		c.Close()
		return nil, err
	}
	return &sshSession{client: c, sess: s, stdin: in, stdout: out}, nil
}

func (s *sshSession) Read(p []byte) (int, error)  { return s.stdout.Read(p) }
func (s *sshSession) Write(p []byte) (int, error) { return s.stdin.Write(p) }
func (s *sshSession) Resize(cols, rows int) error { return s.sess.WindowChange(rows, cols) }
func (s *sshSession) Close() error {
	s.sess.Close()
	return s.client.Close()
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
