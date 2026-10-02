package macos

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/dmdhrumilmistry/vishwakarma/internal/config"
)

// AgentConfig configures `vishwakarma agent`.
type AgentConfig struct {
	// Name identifies the host in the console; defaults to the hostname.
	Name string
	// PublicHost is the address users connect to for forwarded ports (the
	// Mac's LAN IP or DNS name).
	PublicHost string
	// Token must be presented by the server as a bearer token.
	Token string
	// StatePath is the JSON file the agent keeps its VMs in (mode 0600: it
	// holds guest passwords).
	StatePath string
	// BindAddr is where forwarded ports listen, 0.0.0.0 by default.
	BindAddr string
	// PortMin and PortMax bound the host ports used for forwarding.
	PortMin, PortMax int
	// MaxRunning caps VMs running at once; the backend default when 0.
	MaxRunning int
	// AllowImages restricts images to these prefixes; empty allows any.
	AllowImages []string
	Version     string
}

// record is one VM as the agent stores it. Fields are guarded by mu; the
// agent never holds mu while calling the backend.
type record struct {
	VM
	Password     string `json:"password"`
	VNCPassword  string `json:"vncPassword,omitempty"`
	KeepPassword bool   `json:"keepPassword,omitempty"`
	SSHKey       string `json:"sshKey,omitempty"`
	VNC          bool   `json:"vnc"`
	// Desired is "running" or "stopped": what the user last asked for.
	Desired string `json:"desired"`
	// Prepared is set once the image is cloned and configured.
	Prepared bool `json:"prepared,omitempty"`
	// Rotated is set once the image password has been replaced.
	Rotated  bool   `json:"rotated,omitempty"`
	KeyAdded bool   `json:"keyAdded,omitempty"`
	HostKey  string `json:"hostKey,omitempty"`

	mu        sync.Mutex
	run       *Running
	listeners []net.Listener
	cancel    context.CancelFunc
}

func (r *record) running() *Running {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.run
}

func (r *record) sshTarget(addr string) sshTarget {
	r.mu.Lock()
	defer r.mu.Unlock()
	return sshTarget{
		Addr: addr, User: r.User, Password: r.Password, HostKey: r.HostKey,
		OnNewKey: func(k string) {
			r.mu.Lock()
			r.HostKey = k
			r.mu.Unlock()
		},
	}
}

// view snapshots the public fields.
func (r *record) view() VM {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.VM
	v.Ports = append([]int32{}, r.Ports...)
	v.Forwards = append([]Forward{}, r.Forwards...)
	return v
}

func (r *record) set(state, msg string) {
	r.mu.Lock()
	r.State, r.Message = state, msg
	r.mu.Unlock()
}

// Agent manages VMs on one Mac.
type Agent struct {
	cfg AgentConfig
	be  Backend
	log *slog.Logger
	now func() time.Time

	mu  sync.Mutex
	vms map[string]*record
	// saveMu serialises writes of the state file.
	saveMu sync.Mutex
}

// NewAgent loads the state file and returns an agent. Call Run to adopt or
// restart VMs and start the reaper.
func NewAgent(cfg AgentConfig, be Backend, log *slog.Logger) (*Agent, error) {
	if log == nil {
		log = slog.Default()
	}
	if len(cfg.Token) < 24 {
		return nil, errors.New("the agent token must be at least 24 characters")
	}
	if cfg.Name == "" {
		cfg.Name, _ = os.Hostname()
	}
	if cfg.BindAddr == "" {
		cfg.BindAddr = "0.0.0.0"
	}
	if cfg.PortMin == 0 && cfg.PortMax == 0 {
		cfg.PortMin, cfg.PortMax = 20000, 20999
	}
	if cfg.PortMin < 1 || cfg.PortMax > 65535 || cfg.PortMin > cfg.PortMax {
		return nil, fmt.Errorf("invalid port range %d-%d", cfg.PortMin, cfg.PortMax)
	}
	if cfg.MaxRunning <= 0 {
		cfg.MaxRunning = be.DefaultMaxRunning()
	}
	a := &Agent{cfg: cfg, be: be, log: log, now: time.Now, vms: map[string]*record{}}
	if err := a.load(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Agent) load() error {
	if a.cfg.StatePath == "" {
		return nil
	}
	b, err := os.ReadFile(a.cfg.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var recs []*record
	if err := json.Unmarshal(b, &recs); err != nil {
		return fmt.Errorf("read %s: %w", a.cfg.StatePath, err)
	}
	for _, r := range recs {
		a.vms[r.Name] = r
	}
	return nil
}

func (a *Agent) save() {
	if a.cfg.StatePath == "" {
		return
	}
	a.saveMu.Lock()
	defer a.saveMu.Unlock()
	a.mu.Lock()
	recs := make([]*record, 0, len(a.vms))
	for _, r := range a.vms {
		recs = append(recs, r)
	}
	a.mu.Unlock()
	sort.Slice(recs, func(i, j int) bool { return recs[i].Name < recs[j].Name })
	snap := make([]json.RawMessage, 0, len(recs))
	for _, r := range recs {
		r.mu.Lock()
		b, err := json.Marshal(r)
		r.mu.Unlock()
		if err == nil {
			snap = append(snap, b)
		}
	}
	b, _ := json.MarshalIndent(snap, "", "  ")
	if err := os.MkdirAll(filepath.Dir(a.cfg.StatePath), 0o700); err != nil {
		a.log.Error("save state", "err", err)
		return
	}
	tmp := a.cfg.StatePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		a.log.Error("save state", "err", err)
		return
	}
	if err := os.Rename(tmp, a.cfg.StatePath); err != nil {
		a.log.Error("save state", "err", err)
	}
}

// Run adopts or restarts VMs from the state file, then reaps expired VMs
// until ctx ends.
func (a *Agent) Run(ctx context.Context) {
	a.mu.Lock()
	recs := make([]*record, 0, len(a.vms))
	for _, r := range a.vms {
		recs = append(recs, r)
	}
	a.mu.Unlock()
	for _, r := range recs {
		v := r.view()
		switch {
		case v.State == StateDeleting:
			go a.destroy(r)
		case r.Desired == "running" && v.State != StateFailed:
			go a.resume(r)
		default:
			r.set(StateStopped, v.Message)
		}
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.reap()
		}
	}
}

// resume adopts a VM that kept running across an agent restart, or boots it.
func (a *Agent) resume(r *record) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	run, err := a.be.Attach(ctx, r)
	cancel()
	if err != nil {
		a.log.Warn("attach failed, rebooting", "vm", r.Name, "err", err)
	}
	if run != nil {
		a.adopt(r, run, "")
		return
	}
	r.mu.Lock()
	prepared := r.Prepared
	r.mu.Unlock()
	if !prepared {
		a.provision(r)
		return
	}
	a.boot(r)
}

// reap deletes VMs well past their expiry. The server normally deletes
// them on time; this covers a server that is gone.
func (a *Agent) reap() {
	a.mu.Lock()
	var expired []*record
	for _, r := range a.vms {
		v := r.view()
		if !v.ExpiresAt.IsZero() && a.now().After(v.ExpiresAt.Add(10*time.Minute)) && v.State != StateDeleting {
			expired = append(expired, r)
		}
	}
	a.mu.Unlock()
	for _, r := range expired {
		a.log.Info("vm expired", "vm", r.Name)
		r.set(StateDeleting, "expired")
		go a.destroy(r)
	}
}

func (a *Agent) busy() int {
	n := 0
	for _, r := range a.vms {
		v := r.view()
		if r.Desired == "running" && v.State != StateStopped && v.State != StateFailed && v.State != StateDeleting {
			n++
		}
	}
	return n
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Create registers a VM and starts provisioning it in the background.
func (a *Agent) Create(req CreateRequest) (VM, error) {
	if !config.ValidName(req.Name) || !dnsLabel.MatchString(req.Name) {
		return VM{}, &badRequest{"name must be a lowercase DNS label of at most 40 characters"}
	}
	if req.Image == "" || strings.ContainsAny(req.Image, " \t\r\n") {
		return VM{}, &badRequest{"image is required"}
	}
	if len(a.cfg.AllowImages) > 0 {
		ok := false
		for _, p := range a.cfg.AllowImages {
			if strings.HasPrefix(req.Image, p) {
				ok = true
			}
		}
		if !ok {
			return VM{}, &badRequest{"this host does not allow image " + req.Image}
		}
	}
	if req.User == "" {
		return VM{}, &badRequest{"user is required"}
	}
	for _, p := range req.Ports {
		if p < 1 || p > 65535 {
			return VM{}, &badRequest{fmt.Sprintf("port %d is out of range", p)}
		}
	}
	a.mu.Lock()
	if _, ok := a.vms[req.Name]; ok {
		a.mu.Unlock()
		return VM{}, ErrExists
	}
	if a.busy() >= a.cfg.MaxRunning {
		a.mu.Unlock()
		return VM{}, ErrFull
	}
	r := &record{
		VM: VM{
			Name: req.Name, Host: a.cfg.Name, Owner: req.Owner, Template: req.Template, Image: req.Image,
			State: StatePending, Message: "pulling the image", CreatedAt: a.now().UTC().Truncate(time.Second),
			ExpiresAt: req.ExpiresAt, CPU: req.CPU, MemoryMB: req.MemoryMB, DiskGB: req.DiskGB,
			User: req.User, Ports: req.Ports, Forwards: []Forward{}, PublicHost: a.cfg.PublicHost,
			Simulated: a.be.Name() == "simulator",
		},
		Password: req.Password, KeepPassword: req.KeepPassword, SSHKey: req.SSHKey, VNC: req.VNC,
		Desired: "running",
	}
	a.vms[r.Name] = r
	a.mu.Unlock()
	a.save()
	a.log.Info("vm created", "vm", r.Name, "image", r.Image, "owner", r.Owner)
	go a.provision(r)
	return r.view(), nil
}

func (a *Agent) provision(r *record) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	r.mu.Lock()
	r.cancel = cancel
	r.mu.Unlock()
	defer cancel()
	r.set(StatePending, "pulling the image (the first pull of a macOS image can take a long time)")
	if err := a.be.Prepare(ctx, r); err != nil {
		a.failed(r, "preparing the VM failed: "+err.Error())
		return
	}
	r.mu.Lock()
	r.Prepared = true
	stop := r.State == StateDeleting || r.Desired != "running"
	r.mu.Unlock()
	a.save()
	if stop {
		return
	}
	a.boot(r)
}

// failed records an error, unless the operation was cancelled on purpose
// (delete or stop), which reports its own state.
func (a *Agent) failed(r *record, msg string) {
	r.mu.Lock()
	switch {
	case r.State == StateDeleting:
	case r.Desired != "running":
		r.State, r.Message = StateStopped, ""
	default:
		r.State, r.Message = StateFailed, msg
	}
	r.mu.Unlock()
	a.save()
}

func (a *Agent) boot(r *record) {
	r.set(StatePending, "booting")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	r.mu.Lock()
	r.cancel = cancel
	r.mu.Unlock()
	defer cancel()
	run, err := a.be.Start(ctx, r)
	if err != nil {
		a.failed(r, "boot failed: "+err.Error())
		return
	}
	if r.view().State == StateDeleting {
		_ = a.be.Stop(context.Background(), r)
		return
	}
	msg := a.firstBoot(ctx, r, run)
	a.adopt(r, run, msg)
}

// firstBoot replaces the image password and installs the SSH key, once.
func (a *Agent) firstBoot(ctx context.Context, r *record, run *Running) string {
	r.mu.Lock()
	r.run = run // Exec needs it
	user, old, keep, rotated := r.User, r.Password, r.KeepPassword, r.Rotated
	key, keyAdded := r.SSHKey, r.KeyAdded
	r.mu.Unlock()
	var notes []string
	if !keep && !rotated {
		pw := randomHex(8)
		cmd := fmt.Sprintf("dscl . -passwd /Users/%s %s %s", shellQuote(user), shellQuote(old), shellQuote(pw))
		if _, err := a.be.Exec(ctx, r, cmd); err != nil {
			a.log.Warn("password change failed", "vm", r.Name, "err", err)
			notes = append(notes, "the image password is still in use")
		} else {
			r.mu.Lock()
			r.Password, r.Rotated = pw, true
			r.mu.Unlock()
		}
	}
	if key != "" && !keyAdded {
		cmd := "mkdir -p ~/.ssh && chmod 700 ~/.ssh && printf '%s\\n' " + shellQuote(key) + " >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys"
		if _, err := a.be.Exec(ctx, r, cmd); err != nil {
			notes = append(notes, "adding your SSH key failed")
		} else {
			r.mu.Lock()
			r.KeyAdded = true
			r.mu.Unlock()
		}
	}
	return strings.Join(notes, "; ")
}

// adopt marks a booted VM running, opens its forwards and watches it.
func (a *Agent) adopt(r *record, run *Running, msg string) {
	if err := a.openForwards(r, run); err != nil {
		a.log.Error("forwarding failed", "vm", r.Name, "err", err)
		msg = strings.TrimPrefix(msg+"; port forwarding failed: "+err.Error(), "; ")
	}
	r.mu.Lock()
	r.run, r.IP = run, run.IP
	if run.VNCPassword != "" {
		r.VNCPassword = run.VNCPassword
	}
	r.State, r.Message = StateRunning, msg
	r.mu.Unlock()
	a.save()
	a.log.Info("vm running", "vm", r.Name, "ip", run.IP)
	go func() {
		<-run.Done
		r.mu.Lock()
		if r.run != run {
			r.mu.Unlock()
			return
		}
		r.run = nil
		a.closeForwardsLocked(r)
		if r.State != StateDeleting {
			r.State = StateStopped
			r.Message = ""
			if r.Desired == "running" {
				r.Message = "the VM shut down"
				r.Desired = "stopped"
			}
		}
		r.mu.Unlock()
		a.save()
	}()
}

// openForwards listens on host ports for SSH, VNC and the exposed ports.
// A VM keeps its host ports across restarts when they are still free.
func (a *Agent) openForwards(r *record, run *Running) error {
	type want struct {
		name   string
		guest  int32
		target string
	}
	var wants []want
	for _, p := range append([]int32{22}, r.view().Ports...) {
		name := "p" + strconv.Itoa(int(p))
		if p == 22 {
			name = ForwardSSH
		}
		if t, ok := run.Targets[p]; ok {
			wants = append(wants, want{name, p, t})
		}
	}
	if run.VNC != "" {
		wants = append(wants, want{ForwardVNC, 5900, run.VNC})
	}

	a.mu.Lock()
	used := map[int32]bool{}
	for _, o := range a.vms {
		if o == r {
			continue
		}
		for _, f := range o.view().Forwards {
			used[f.HostPort] = true
		}
	}
	prev := map[string]int32{}
	for _, f := range r.view().Forwards {
		prev[f.Name] = f.HostPort
	}
	var fwds []Forward
	var lns []net.Listener
	var err error
	for _, w := range wants {
		var ln net.Listener
		ports := []int32{}
		if p, ok := prev[w.name]; ok {
			ports = append(ports, p)
		}
		for p := a.cfg.PortMin; p <= a.cfg.PortMax; p++ {
			ports = append(ports, int32(p))
		}
		for _, p := range ports {
			if used[p] {
				continue
			}
			l, lerr := net.Listen("tcp", net.JoinHostPort(a.cfg.BindAddr, strconv.Itoa(int(p))))
			if lerr == nil {
				ln = l
				used[p] = true
				fwds = append(fwds, Forward{Name: w.name, GuestPort: w.guest, HostPort: p})
				break
			}
		}
		if ln == nil {
			err = fmt.Errorf("no free host port in %d-%d", a.cfg.PortMin, a.cfg.PortMax)
			break
		}
		lns = append(lns, ln)
		go proxy(ln, w.target)
	}
	a.mu.Unlock()

	r.mu.Lock()
	a.closeForwardsLocked(r)
	r.Forwards, r.listeners = fwds, lns
	r.mu.Unlock()
	return err
}

func (a *Agent) closeForwardsLocked(r *record) {
	for _, l := range r.listeners {
		l.Close()
	}
	r.listeners = nil
}

// proxy relays every connection on ln to target.
func proxy(ln net.Listener, target string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			u, err := net.DialTimeout("tcp", target, 10*time.Second)
			if err != nil {
				return
			}
			defer u.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(u, c); done <- struct{}{} }()
			go func() { _, _ = io.Copy(c, u); done <- struct{}{} }()
			<-done
		}()
	}
}

func (a *Agent) get(name string) (*record, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.vms[name]
	if !ok {
		return nil, ErrNotFound
	}
	return r, nil
}

// Stop shuts a VM down; it keeps its disk.
func (a *Agent) Stop(name string) (VM, error) {
	r, err := a.get(name)
	if err != nil {
		return VM{}, err
	}
	v := r.view()
	if v.State == StateDeleting {
		return v, ErrBusy
	}
	r.mu.Lock()
	r.Desired = "stopped"
	run, cancel := r.run, r.cancel
	r.mu.Unlock()
	if run == nil {
		if cancel != nil && v.State == StatePending {
			cancel()
		}
		r.set(StateStopped, "")
		a.save()
		return r.view(), nil
	}
	r.set(StatePending, "stopping")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := a.be.Stop(ctx, r); err != nil {
			a.log.Error("stop failed", "vm", name, "err", err)
		}
	}()
	return r.view(), nil
}

// Start boots a stopped VM.
func (a *Agent) Start(name string) (VM, error) {
	r, err := a.get(name)
	if err != nil {
		return VM{}, err
	}
	v := r.view()
	if v.State == StateRunning || (v.State == StatePending && r.Desired == "running") {
		return v, nil
	}
	if v.State == StateDeleting || v.Message == "stopping" {
		return v, ErrBusy
	}
	a.mu.Lock()
	if a.busy() >= a.cfg.MaxRunning {
		a.mu.Unlock()
		return v, ErrFull
	}
	r.mu.Lock()
	r.Desired = "running"
	failedPrepare := !r.Prepared
	r.State, r.Message = StatePending, "booting"
	r.mu.Unlock()
	a.mu.Unlock()
	a.save()
	if failedPrepare {
		go a.provision(r)
	} else {
		go a.boot(r)
	}
	return r.view(), nil
}

// Delete removes a VM and its disk.
func (a *Agent) Delete(name string) error {
	r, err := a.get(name)
	if err != nil {
		return err
	}
	r.set(StateDeleting, "deleting")
	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Unlock()
	a.save()
	go a.destroy(r)
	return nil
}

func (a *Agent) destroy(r *record) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := a.be.Delete(ctx, r); err != nil {
		a.log.Error("delete failed", "vm", r.Name, "err", err)
		r.set(StateFailed, "delete failed: "+err.Error())
		a.save()
		return
	}
	r.mu.Lock()
	r.run = nil
	a.closeForwardsLocked(r)
	r.mu.Unlock()
	a.mu.Lock()
	delete(a.vms, r.Name)
	a.mu.Unlock()
	a.save()
	a.log.Info("vm deleted", "vm", r.Name)
}

// Extend sets a new expiry.
func (a *Agent) Extend(name string, at time.Time) (VM, error) {
	r, err := a.get(name)
	if err != nil {
		return VM{}, err
	}
	r.mu.Lock()
	r.ExpiresAt = at
	r.mu.Unlock()
	a.save()
	return r.view(), nil
}

// List returns every VM.
func (a *Agent) List() []VM {
	a.mu.Lock()
	recs := make([]*record, 0, len(a.vms))
	for _, r := range a.vms {
		recs = append(recs, r)
	}
	a.mu.Unlock()
	out := make([]VM, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.view())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Info describes the host.
func (a *Agent) Info() Info {
	a.mu.Lock()
	defer a.mu.Unlock()
	running := 0
	for _, r := range a.vms {
		if r.view().State == StateRunning {
			running++
		}
	}
	return Info{
		Name: a.cfg.Name, Version: a.cfg.Version, Backend: a.be.Name(), PublicHost: a.cfg.PublicHost,
		MaxRunning: a.cfg.MaxRunning, Running: running, VMs: len(a.vms),
	}
}

type badRequest struct{ msg string }

func (e *badRequest) Error() string { return e.msg }

// Handler is the agent HTTP API. Everything but /healthz needs the token.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /v1/info", a.authed(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.Info()) }))
	mux.HandleFunc("GET /v1/vms", a.authed(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.List()) }))
	mux.HandleFunc("POST /v1/vms", a.authed(func(w http.ResponseWriter, r *http.Request) {
		var req CreateRequest
		if !readJSON(w, r, &req) {
			return
		}
		vm, err := a.Create(req)
		reply(w, http.StatusCreated, vm, err)
	}))
	mux.HandleFunc("GET /v1/vms/{name}", a.authed(func(w http.ResponseWriter, r *http.Request) {
		rec, err := a.get(r.PathValue("name"))
		if err != nil {
			reply(w, 0, nil, err)
			return
		}
		writeJSON(w, 200, rec.view())
	}))
	mux.HandleFunc("DELETE /v1/vms/{name}", a.authed(func(w http.ResponseWriter, r *http.Request) {
		if err := a.Delete(r.PathValue("name")); err != nil {
			reply(w, 0, nil, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /v1/vms/{name}/stop", a.authed(func(w http.ResponseWriter, r *http.Request) {
		vm, err := a.Stop(r.PathValue("name"))
		reply(w, 200, vm, err)
	}))
	mux.HandleFunc("POST /v1/vms/{name}/start", a.authed(func(w http.ResponseWriter, r *http.Request) {
		vm, err := a.Start(r.PathValue("name"))
		reply(w, 200, vm, err)
	}))
	mux.HandleFunc("POST /v1/vms/{name}/extend", a.authed(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ExpiresAt time.Time `json:"expiresAt"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		vm, err := a.Extend(r.PathValue("name"), body.ExpiresAt)
		reply(w, 200, vm, err)
	}))
	mux.HandleFunc("GET /v1/vms/{name}/credentials", a.authed(func(w http.ResponseWriter, r *http.Request) {
		rec, err := a.get(r.PathValue("name"))
		if err != nil {
			reply(w, 0, nil, err)
			return
		}
		rec.mu.Lock()
		c := Credentials{User: rec.User, Password: rec.Password, VNCPassword: rec.VNCPassword}
		rec.mu.Unlock()
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, c)
	}))
	mux.HandleFunc("GET /v1/vms/{name}/terminal", a.authed(a.terminal))
	mux.HandleFunc("GET /v1/vms/{name}/vnc", a.authed(a.vnc))
	return mux
}

func (a *Agent) authed(h http.HandlerFunc) http.HandlerFunc {
	want := sha256.Sum256([]byte("Bearer " + a.cfg.Token))
	return func(w http.ResponseWriter, r *http.Request) {
		got := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
			writeError(w, http.StatusUnauthorized, "missing or wrong agent token")
			return
		}
		h(w, r)
	}
}

// The agent is called by the server only; there is no browser origin.
var agentUpgrader = websocket.Upgrader{ReadBufferSize: 32 << 10, WriteBufferSize: 32 << 10}

// terminal bridges a WebSocket (same protocol as the server's) to a shell
// in the guest.
func (a *Agent) terminal(w http.ResponseWriter, req *http.Request) {
	r, err := a.get(req.PathValue("name"))
	if err != nil {
		reply(w, 0, nil, err)
		return
	}
	if r.running() == nil {
		writeError(w, http.StatusConflict, "the VM is not running")
		return
	}
	cols, _ := strconv.Atoi(req.URL.Query().Get("cols"))
	rows, _ := strconv.Atoi(req.URL.Query().Get("rows"))
	ws, err := agentUpgrader.Upgrade(w, req, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess, err := a.be.Shell(ctx, r, cols, rows)
	if err != nil {
		_ = ws.WriteMessage(websocket.TextMessage, []byte("could not open a shell: "+err.Error()))
		return
	}
	defer sess.Close()

	go func() {
		defer cancel()
		defer sess.Close()
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.TextMessage {
				var m struct {
					Type string `json:"type"`
					Cols int    `json:"cols"`
					Rows int    `json:"rows"`
				}
				if json.Unmarshal(data, &m) == nil && m.Type == "resize" && m.Cols > 0 && m.Rows > 0 {
					_ = sess.Resize(m.Cols, m.Rows)
				}
				continue
			}
			if _, err := sess.Write(data); err != nil {
				return
			}
		}
	}()
	buf := make([]byte, 32<<10)
	for {
		n, err := sess.Read(buf)
		if n > 0 {
			if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			if ctx.Err() == nil {
				_ = ws.WriteMessage(websocket.TextMessage, []byte("session ended"))
			}
			_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
			return
		}
	}
}

// vnc relays raw RFB between a WebSocket (binary frames) and the VM's VNC
// server, for the console's Screen tab.
func (a *Agent) vnc(w http.ResponseWriter, req *http.Request) {
	r, err := a.get(req.PathValue("name"))
	if err != nil {
		reply(w, 0, nil, err)
		return
	}
	run := r.running()
	if run == nil || run.VNC == "" {
		writeError(w, http.StatusConflict, "the VM has no screen right now; start it (restart it if the agent restarted)")
		return
	}
	conn, err := net.DialTimeout("tcp", run.VNC, 10*time.Second)
	if err != nil {
		writeError(w, http.StatusBadGateway, "VNC: "+err.Error())
		return
	}
	defer conn.Close()
	ws, err := agentUpgrader.Upgrade(w, req, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	BridgeTCP(ws, conn)
}

// BridgeTCP copies binary WebSocket frames to conn and back until either
// side closes.
func BridgeTCP(ws *websocket.Conn, conn net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.BinaryMessage {
				continue
			}
			if _, err := conn.Write(data); err != nil {
				return
			}
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 64<<10)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	<-done
}

// BridgeWS copies frames between two WebSockets until either side closes.
func BridgeWS(a, b *websocket.Conn) {
	done := make(chan struct{}, 2)
	pipe := func(src, dst *websocket.Conn) {
		defer func() { done <- struct{}{} }()
		for {
			mt, data, err := src.ReadMessage()
			if err != nil {
				return
			}
			if err := dst.WriteMessage(mt, data); err != nil {
				return
			}
		}
	}
	go pipe(a, b)
	go pipe(b, a)
	<-done
}

func reply(w http.ResponseWriter, status int, v any, err error) {
	var bad *badRequest
	switch {
	case err == nil:
		writeJSON(w, status, v)
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, bad.msg)
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrExists), errors.Is(err, ErrFull), errors.Is(err, ErrBusy):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
