package macos

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// testSSHServer accepts user/password, answers exec requests by echoing the
// command, and runs a one-line "shell" for pty sessions.
type testSSHServer struct {
	addr  string
	mu    sync.Mutex
	execs []string
}

func newTestSSHServer(t *testing.T, user, password string) *testSSHServer {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		// Only keyboard-interactive, like a stock macOS sshd.
		KeyboardInteractiveCallback: func(c ssh.ConnMetadata, ch ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			ans, err := ch("", "", []string{"Password:"}, []bool{false})
			if err != nil || c.User() != user || len(ans) != 1 || ans[0] != password {
				return nil, fmt.Errorf("denied")
			}
			return nil, nil
		},
	}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &testSSHServer{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c, cfg)
		}
	}()
	return s
}

func (s *testSSHServer) serve(c net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer ch.Close()
			for r := range creqs {
				switch r.Type {
				case "exec":
					var p struct{ Cmd string }
					_ = ssh.Unmarshal(r.Payload, &p)
					s.mu.Lock()
					s.execs = append(s.execs, p.Cmd)
					s.mu.Unlock()
					_ = r.Reply(true, nil)
					_, _ = io.WriteString(ch, "ran: "+p.Cmd)
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
					return
				case "pty-req", "window-change":
					_ = r.Reply(true, nil)
				case "shell":
					_ = r.Reply(true, nil)
					_, _ = io.WriteString(ch, "admin@mac ~ % ")
					buf := make([]byte, 64)
					n, _ := ch.Read(buf)
					_, _ = io.WriteString(ch, "you typed "+strings.TrimSpace(string(buf[:n])))
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
					return
				default:
					_ = r.Reply(false, nil)
				}
			}
		}()
	}
}

func TestSSHExecAndHostKeyPinning(t *testing.T) {
	srv := newTestSSHServer(t, "admin", "pw")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var pinned string
	tgt := sshTarget{Addr: srv.addr, User: "admin", Password: "pw", OnNewKey: func(k string) { pinned = k }}
	out, err := sshExec(ctx, tgt, "dscl . -passwd /Users/admin 'pw' 'new'")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "dscl . -passwd /Users/admin") || pinned == "" {
		t.Fatalf("exec output %q, pinned %q", out, pinned)
	}

	tgt.HostKey = pinned
	if _, err := sshExec(ctx, tgt, "true"); err != nil {
		t.Fatalf("pinned key rejected: %v", err)
	}
	tgt.HostKey = "c29tZXRoaW5nIGVsc2U="
	if _, err := sshExec(ctx, tgt, "true"); err == nil {
		t.Fatal("a different host key was accepted")
	}

	bad := sshTarget{Addr: srv.addr, User: "admin", Password: "wrong"}
	if _, err := sshExec(ctx, bad, "true"); err == nil {
		t.Fatal("wrong password accepted")
	}
}

func TestSSHShell(t *testing.T) {
	srv := newTestSSHServer(t, "admin", "pw")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := sshShell(ctx, sshTarget{Addr: srv.addr, User: "admin", Password: "pw"}, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Write([]byte("sw_vers\n")); err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(sess)
	if !strings.Contains(string(b), "you typed sw_vers") {
		t.Fatalf("shell output %q", b)
	}
}

func TestWaitSSHTimesOut(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close() // nothing listens there now
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := waitSSH(ctx, sshTarget{Addr: addr, User: "a", Password: "b"}); err == nil {
		t.Fatal("waitSSH succeeded without a server")
	}
}
