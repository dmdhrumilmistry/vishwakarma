package macos

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Tart is the Backend for real Macs. It shells out to the tart CLI
// (https://tart.run), which wraps Apple's Virtualization.framework.
//
// The agent must run in a logged-in user session (a LaunchAgent, not a
// LaunchDaemon): Virtualization.framework refuses to start guests outside
// one.
type Tart struct {
	// Bin is the tart executable; "tart" from PATH by default.
	Bin string
	// BootTimeout bounds waiting for the guest IP and SSH.
	BootTimeout time.Duration

	mu    sync.Mutex
	procs map[string]*exec.Cmd
}

// NewTart returns a Tart backend.
func NewTart(bin string) *Tart {
	if bin == "" {
		bin = "tart"
	}
	return &Tart{Bin: bin, BootTimeout: 5 * time.Minute, procs: map[string]*exec.Cmd{}}
}

func (t *Tart) Name() string { return "tart" }

// DefaultMaxRunning is two: the macOS license allows two guest instances
// per Mac and Virtualization.framework enforces it.
func (t *Tart) DefaultMaxRunning() int { return 2 }

func (t *Tart) cmd(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, t.Bin, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("tart %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (t *Tart) Prepare(ctx context.Context, r *record) error {
	name := tartName(r.Name)
	// A leftover from a crashed agent would make clone fail.
	_, _ = t.cmd(ctx, "delete", name)
	if _, err := t.cmd(ctx, "clone", r.Image, name); err != nil {
		return err
	}
	args := []string{"set", name}
	if r.CPU > 0 {
		args = append(args, "--cpu", strconv.Itoa(r.CPU))
	}
	if r.MemoryMB > 0 {
		args = append(args, "--memory", strconv.Itoa(r.MemoryMB))
	}
	if r.DiskGB > 0 {
		// Tart can only grow a disk; asking for less than the image fails.
		args = append(args, "--disk-size", strconv.Itoa(r.DiskGB))
	}
	if len(args) > 2 {
		if _, err := t.cmd(ctx, args...); err != nil {
			return err
		}
	}
	return nil
}

// vncURL matches the address tart prints for --vnc-experimental, e.g.
// "vnc://:secret@127.0.0.1:59322".
var vncURL = regexp.MustCompile(`vnc://(?:[^:@/\s]*:([^@\s]*)@)?([^:/\s]+):(\d+)`)

// parseVNC extracts the dial address and password from a tart output line.
func parseVNC(line string) (addr, password string, ok bool) {
	m := vncURL.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	return m[2] + ":" + m[3], m[1], true
}

func (t *Tart) Start(ctx context.Context, r *record) (*Running, error) {
	name := tartName(r.Name)
	args := []string{"run", name, "--no-graphics"}
	if r.VNC {
		// Virtualization.framework's own VNC server: needs nothing in the guest.
		args = append(args, "--vnc-experimental")
	}
	// Not CommandContext: the VM must outlive the request that started it.
	cmd := exec.Command(t.Bin, args...)
	detach(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("tart run: %w", err)
	}
	t.mu.Lock()
	t.procs[r.Name] = cmd
	t.mu.Unlock()

	done := make(chan struct{})
	vnc := make(chan [2]string, 1)
	var tail []string
	var tailMu sync.Mutex
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			if addr, pw, ok := parseVNC(line); ok {
				select {
				case vnc <- [2]string{addr, pw}:
				default:
				}
			}
			tailMu.Lock()
			if tail = append(tail, line); len(tail) > 5 {
				tail = tail[1:]
			}
			tailMu.Unlock()
		}
	}()
	go func() {
		_ = cmd.Wait()
		t.mu.Lock()
		if t.procs[r.Name] == cmd {
			delete(t.procs, r.Name)
		}
		t.mu.Unlock()
		close(done)
	}()

	bootCtx, cancel := context.WithTimeout(ctx, t.BootTimeout)
	defer cancel()
	ip, err := t.ip(bootCtx, name, done)
	if err != nil {
		_ = t.Stop(context.Background(), r)
		tailMu.Lock()
		defer tailMu.Unlock()
		return nil, fmt.Errorf("%w (tart said: %s)", err, strings.Join(tail, " | "))
	}
	run := &Running{IP: ip, Targets: targets(ip, r.Ports), Done: done}
	if r.VNC {
		select {
		case v := <-vnc:
			run.VNC, run.VNCPassword = v[0], v[1]
		case <-time.After(30 * time.Second):
		}
	}
	if err := waitSSH(bootCtx, r.sshTarget(run.Targets[22])); err != nil {
		_ = t.Stop(context.Background(), r)
		return nil, err
	}
	return run, nil
}

// ip waits for the guest to get an address, failing early if tart exits.
func (t *Tart) ip(ctx context.Context, name string, done <-chan struct{}) (string, error) {
	for {
		out, err := t.cmd(ctx, "ip", name, "--wait", "10")
		if err == nil {
			if ip := strings.TrimSpace(out); ip != "" {
				return ip, nil
			}
		}
		select {
		case <-done:
			return "", errors.New("the VM exited while booting")
		case <-ctx.Done():
			return "", fmt.Errorf("no IP address for the VM: %w", ctx.Err())
		default:
		}
	}
}

func targets(ip string, ports []int32) map[int32]string {
	m := map[int32]string{22: fmt.Sprintf("%s:22", ip)}
	for _, p := range ports {
		m[p] = fmt.Sprintf("%s:%d", ip, p)
	}
	return m
}

// tartVM is one entry of `tart list --format json`. Older releases report
// Running, newer ones State.
type tartVM struct {
	Name    string `json:"Name"`
	State   string `json:"State"`
	Running bool   `json:"Running"`
}

func (v tartVM) running() bool { return v.Running || strings.EqualFold(v.State, "running") }

func parseList(out []byte) ([]tartVM, error) {
	var vms []tartVM
	if err := json.Unmarshal(out, &vms); err != nil {
		return nil, fmt.Errorf("parse tart list: %w", err)
	}
	return vms, nil
}

func (t *Tart) Attach(ctx context.Context, r *record) (*Running, error) {
	out, err := t.cmd(ctx, "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	vms, err := parseList([]byte(out))
	if err != nil {
		return nil, err
	}
	name := tartName(r.Name)
	for _, v := range vms {
		if v.Name != name || !v.running() {
			continue
		}
		ip, err := t.cmd(ctx, "ip", name, "--wait", "30")
		if err != nil {
			return nil, err
		}
		done := make(chan struct{})
		go t.watch(name, done)
		// The VNC port of the old tart process is unknown; restart the VM to
		// get the screen back.
		return &Running{IP: strings.TrimSpace(ip), Targets: targets(strings.TrimSpace(ip), r.Ports), Done: done}, nil
	}
	return nil, nil
}

// watch closes done when an adopted VM stops.
func (t *Tart) watch(name string, done chan struct{}) {
	defer close(done)
	for {
		time.Sleep(10 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		out, err := t.cmd(ctx, "list", "--format", "json")
		cancel()
		if err != nil {
			continue
		}
		vms, err := parseList([]byte(out))
		if err != nil {
			continue
		}
		up := false
		for _, v := range vms {
			if v.Name == name && v.running() {
				up = true
			}
		}
		if !up {
			return
		}
	}
}

func (t *Tart) Stop(ctx context.Context, r *record) error {
	_, err := t.cmd(ctx, "stop", tartName(r.Name), "--timeout", "30")
	t.mu.Lock()
	cmd := t.procs[r.Name]
	t.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	if err != nil && strings.Contains(err.Error(), "not running") {
		return nil
	}
	return err
}

func (t *Tart) Delete(ctx context.Context, r *record) error {
	_ = t.Stop(ctx, r)
	_, err := t.cmd(ctx, "delete", tartName(r.Name))
	if err != nil && (strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "not found")) {
		return nil
	}
	return err
}

func (t *Tart) Exec(ctx context.Context, r *record, cmd string) (string, error) {
	run := r.running()
	if run == nil {
		return "", errors.New("the VM is not running")
	}
	return sshExec(ctx, r.sshTarget(run.Targets[22]), cmd)
}

func (t *Tart) Shell(ctx context.Context, r *record, cols, rows int) (Session, error) {
	run := r.running()
	if run == nil {
		return nil, errors.New("the VM is not running")
	}
	return sshShell(ctx, r.sshTarget(run.Targets[22]), cols, rows)
}
