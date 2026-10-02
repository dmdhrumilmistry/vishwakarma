package macos

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// Simulator is a Backend without real guests, for clusters without a Mac.
// VMs go through the same lifecycle, every forwarded port answers with a
// banner, and the terminal is a tiny fake shell. It exists so the console,
// API, agent protocol and networking can be tested on Linux.
type Simulator struct {
	// PrepareDelay imitates pulling the image.
	PrepareDelay time.Duration

	mu   sync.Mutex
	runs map[string]*simRun
}

type simRun struct {
	listeners []net.Listener
	done      chan struct{}
}

// NewSimulator returns a simulator backend.
func NewSimulator() *Simulator {
	return &Simulator{PrepareDelay: 3 * time.Second, runs: map[string]*simRun{}}
}

func (s *Simulator) Name() string           { return "simulator" }
func (s *Simulator) DefaultMaxRunning() int { return 4 }

func (s *Simulator) Prepare(ctx context.Context, r *record) error {
	select {
	case <-time.After(s.PrepareDelay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Simulator) Start(ctx context.Context, r *record) (*Running, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.runs[r.Name]; ok {
		return nil, fmt.Errorf("%s is already running", r.Name)
	}
	run := &simRun{done: make(chan struct{})}
	out := &Running{IP: "127.0.0.1", Targets: map[int32]string{}, Done: run.done}
	ports := append([]int32{22}, r.Ports...)
	if r.VNC {
		ports = append(ports, 5900)
	}
	for _, p := range ports {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			for _, l := range run.listeners {
				l.Close()
			}
			return nil, err
		}
		run.listeners = append(run.listeners, ln)
		go serveBanner(ln, banner(p))
		if p == 5900 && r.VNC {
			out.VNC = ln.Addr().String()
			out.VNCPassword = randomHex(4)
		} else {
			out.Targets[p] = ln.Addr().String()
		}
	}
	s.runs[r.Name] = run
	return out, nil
}

// Attach finds nothing: simulated VMs do not survive an agent restart.
func (s *Simulator) Attach(ctx context.Context, r *record) (*Running, error) { return nil, nil }

func (s *Simulator) Stop(ctx context.Context, r *record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if run, ok := s.runs[r.Name]; ok {
		for _, l := range run.listeners {
			l.Close()
		}
		close(run.done)
		delete(s.runs, r.Name)
	}
	return nil
}

func (s *Simulator) Delete(ctx context.Context, r *record) error { return s.Stop(ctx, r) }

// Exec accepts the password change and SSH key commands the agent sends.
func (s *Simulator) Exec(ctx context.Context, r *record, cmd string) (string, error) { return "", nil }

func (s *Simulator) Shell(ctx context.Context, r *record, cols, rows int) (Session, error) {
	return newFakeShell(r.User, r.Name), nil
}

func banner(port int32) string {
	switch port {
	case 22:
		return "SSH-2.0-Vishwakarma_Simulator\r\n"
	case 5900:
		return "RFB 003.008\n"
	}
	return fmt.Sprintf("vishwakarma simulator: guest port %d\n", port)
}

func serveBanner(ln net.Listener, msg string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			_, _ = io.WriteString(c, msg)
		}()
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// fakeShell answers a handful of commands like a macOS zsh would.
type fakeShell struct {
	user, host string
	out        *io.PipeReader
	w          *io.PipeWriter
	line       []byte
	mu         sync.Mutex
	closed     bool
}

func newFakeShell(user, host string) *fakeShell {
	r, w := io.Pipe()
	s := &fakeShell{user: user, host: host, out: r, w: w}
	go func() {
		s.print("Simulated macOS VM: there is no real guest behind this terminal.\r\n" +
			"Try: sw_vers, uname -a, hostname, whoami, echo, exit\r\n\r\n")
		s.prompt()
	}()
	return s
}

func (s *fakeShell) print(t string) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if !closed {
		_, _ = s.w.Write([]byte(t))
	}
}

func (s *fakeShell) prompt() { s.print(fmt.Sprintf("%s@%s ~ %% ", s.user, s.host)) }

func (s *fakeShell) Read(p []byte) (int, error) { return s.out.Read(p) }

func (s *fakeShell) Write(p []byte) (int, error) {
	for _, b := range p {
		switch b {
		case '\r', '\n':
			s.print("\r\n")
			if s.run(strings.TrimSpace(string(s.line))) {
				s.Close()
				return len(p), nil
			}
			s.line = s.line[:0]
			s.prompt()
		case 0x7f, 0x08:
			if len(s.line) > 0 {
				s.line = s.line[:len(s.line)-1]
				s.print("\b \b")
			}
		case 0x03:
			s.line = s.line[:0]
			s.print("^C\r\n")
			s.prompt()
		default:
			if b >= 0x20 {
				s.line = append(s.line, b)
				s.print(string(b))
			}
		}
	}
	return len(p), nil
}

// run executes one command line and reports whether the shell should exit.
func (s *fakeShell) run(cmd string) bool {
	name, rest, _ := strings.Cut(cmd, " ")
	switch name {
	case "":
	case "exit", "logout":
		s.print("logout\r\n")
		return true
	case "sw_vers":
		s.print("ProductName:\t\tmacOS\r\nProductVersion:\t\t15.0 (simulated)\r\nBuildVersion:\t\tSIM\r\n")
	case "uname":
		if strings.Contains(rest, "-a") {
			s.print(fmt.Sprintf("Darwin %s 24.0.0 Darwin Kernel Version 24.0.0 (simulated) arm64\r\n", s.host))
		} else {
			s.print("Darwin\r\n")
		}
	case "hostname":
		s.print(s.host + "\r\n")
	case "whoami":
		s.print(s.user + "\r\n")
	case "echo":
		s.print(rest + "\r\n")
	default:
		s.print(fmt.Sprintf("zsh: command not found: %s (this is a simulated VM)\r\n", name))
	}
	return false
}

func (s *fakeShell) Resize(cols, rows int) error { return nil }

func (s *fakeShell) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.w.Close()
	}
	return nil
}
