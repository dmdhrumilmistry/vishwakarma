package sandbox

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/dmdhrumilmistry/vishwakarma/internal/config"
	"github.com/dmdhrumilmistry/vishwakarma/internal/macos"
)

// SetMacOS enables macOS sandboxes through the given agent pool.
func (m *Manager) SetMacOS(p *macos.Pool) { m.mac = p }

// MacOS returns the agent pool, nil when macOS is not configured.
func (m *Manager) MacOS() *macos.Pool { return m.mac }

// MacOSAvailable reports whether at least one Mac host answers.
func (m *Manager) MacOSAvailable(ctx context.Context) bool {
	if m.mac == nil {
		return false
	}
	m.macMu.Lock()
	defer m.macMu.Unlock()
	if m.now().Sub(m.macChecked) > 30*time.Second {
		m.macCached = false
		for _, h := range m.mac.Hosts(ctx) {
			if h.Error == "" {
				m.macCached = true
			}
		}
		m.macChecked = m.now()
	}
	return m.macCached
}

func (m *Manager) createMac(ctx context.Context, p *plan) error {
	mb := int(p.Memory.Value() / (1 << 20))
	cpu := int(math.Ceil(float64(p.CPU.MilliValue()) / 1000))
	req := macos.CreateRequest{
		Name: p.Name, Owner: p.Owner, Template: p.Template, Image: p.Image,
		CPU: cpu, MemoryMB: mb, User: p.User, Password: p.Password, KeepPassword: p.KeepPassword,
		SSHKey: p.SSHKey, Ports: p.Ports, VNC: m.policy.MacOS.VNC, ExpiresAt: p.ExpiresAt.UTC(),
	}
	if p.Disk != nil {
		req.DiskGB = int(math.Ceil(float64(p.Disk.Value()) / (1 << 30)))
	}
	_, err := m.mac.Create(ctx, req)
	return macErr(err)
}

// macErr turns agent replies into the manager's error types.
func macErr(err error) error {
	var ae *macos.AgentError
	if errors.As(err, &ae) {
		switch ae.Status {
		case http.StatusNotFound:
			return ErrNotFound
		case http.StatusBadRequest, http.StatusConflict:
			return invalid("%s", ae.Msg)
		}
	}
	if errors.Is(err, macos.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// macList returns macOS sandboxes matching the label filter used for
// Kubernetes objects (owner and sandbox name).
func (m *Manager) macList(ctx context.Context, set labels.Set) []Sandbox {
	if m.mac == nil {
		return nil
	}
	vms, err := m.mac.List(ctx)
	if err != nil {
		// One unreachable Mac must not hide everything else.
		m.log.Warn("listing macOS VMs", "err", err)
	}
	var out []Sandbox
	for _, vm := range vms {
		if v, ok := set[LabelOwner]; ok && OwnerLabel(vm.Owner) != v {
			continue
		}
		if v, ok := set[LabelSandbox]; ok && vm.Name != v {
			continue
		}
		out = append(out, fromMacVM(vm))
	}
	return out
}

func fromMacVM(vm macos.VM) Sandbox {
	s := Sandbox{
		Name: vm.Name, Kind: config.KindMacOS, Template: vm.Template, Image: vm.Image, Owner: vm.Owner,
		Status: vm.State, Message: vm.Message, CreatedAt: vm.CreatedAt, ExpiresAt: vm.ExpiresAt,
		CPU: fmt.Sprint(vm.CPU), Memory: fmt.Sprintf("%dGi", vm.MemoryMB/1024), User: vm.User,
		Node: vm.Host, Host: vm.Host, IP: vm.IP, Expose: ExposeHost, Simulated: vm.Simulated,
		Endpoints: []Endpoint{},
	}
	if vm.MemoryMB%1024 != 0 {
		s.Memory = fmt.Sprintf("%dMi", vm.MemoryMB)
	}
	if vm.DiskGB > 0 {
		s.Disk = fmt.Sprintf("%dGi", vm.DiskGB)
	}
	switch s.Status {
	case StatusPending, StatusRunning, StatusStopped, StatusFailed, StatusTerminating:
	default:
		s.Status = StatusPending
	}
	host := vm.PublicHost
	if host == "" {
		host = vm.Host
	}
	for _, f := range vm.Forwards {
		name := ""
		if f.Name == macos.ForwardSSH || f.Name == macos.ForwardVNC {
			name = f.Name
		}
		s.Endpoints = append(s.Endpoints, Endpoint{
			Name: name, Port: f.GuestPort, NodePort: f.HostPort, Protocol: "TCP",
			Address: fmt.Sprintf("%s:%d", host, f.HostPort),
		})
	}
	return s
}
