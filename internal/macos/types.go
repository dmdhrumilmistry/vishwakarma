// Package macos runs macOS sandboxes on Mac hosts.
//
// macOS guests need Apple's Virtualization.framework, so they cannot run as
// Kubernetes pods. Each Mac runs `vishwakarma agent`, which drives Tart
// (https://tart.run) and exposes a small HTTP API; the server in the cluster
// calls it through a Pool. The agent also forwards guest ports (SSH, VNC,
// anything the user exposed) to host ports, and bridges the web terminal to
// SSH in the guest.
//
// For clusters without a Mac (development, CI, a Linux lab) the agent has a
// simulator backend: the whole lifecycle, port forwarding and terminal work,
// but there is no real guest.
package macos

import (
	"errors"
	"time"
)

// VM states reported by the agent. They match the sandbox statuses.
const (
	StatePending  = "Pending"
	StateRunning  = "Running"
	StateStopped  = "Stopped"
	StateFailed   = "Failed"
	StateDeleting = "Terminating"
)

// Forward names for well-known guest ports.
const (
	ForwardSSH = "ssh"
	ForwardVNC = "vnc"
)

// VM is the public view of a VM on an agent. It never carries passwords.
type VM struct {
	Name      string    `json:"name"`
	Host      string    `json:"host"`
	Owner     string    `json:"owner"`
	Template  string    `json:"template,omitempty"`
	Image     string    `json:"image"`
	State     string    `json:"state"`
	Message   string    `json:"message,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	CPU       int       `json:"cpu"`
	MemoryMB  int       `json:"memoryMB"`
	DiskGB    int       `json:"diskGB,omitempty"`
	User      string    `json:"user"`
	IP        string    `json:"ip,omitempty"`
	Ports     []int32   `json:"ports"`
	Forwards  []Forward `json:"forwards"`
	// PublicHost is the address clients use for forwarded ports.
	PublicHost string `json:"publicHost"`
	Simulated  bool   `json:"simulated,omitempty"`
}

// Forward maps a guest port to a port on the Mac host.
type Forward struct {
	Name      string `json:"name"`
	GuestPort int32  `json:"guestPort"`
	HostPort  int32  `json:"hostPort"`
}

// CreateRequest asks an agent for a new VM.
type CreateRequest struct {
	Name     string `json:"name"`
	Owner    string `json:"owner"`
	Template string `json:"template,omitempty"`
	Image    string `json:"image"`
	CPU      int    `json:"cpu"`
	MemoryMB int    `json:"memoryMB"`
	DiskGB   int    `json:"diskGB,omitempty"`
	User     string `json:"user"`
	Password string `json:"password"`
	// KeepPassword skips replacing the image password on first boot.
	KeepPassword bool      `json:"keepPassword,omitempty"`
	SSHKey       string    `json:"sshKey,omitempty"`
	Ports        []int32   `json:"ports,omitempty"`
	VNC          bool      `json:"vnc"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

// Credentials are the logins of a VM.
type Credentials struct {
	User        string `json:"user"`
	Password    string `json:"password"`
	VNCPassword string `json:"vncPassword,omitempty"`
}

// Info describes an agent.
type Info struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Backend    string `json:"backend"`
	PublicHost string `json:"publicHost"`
	// MaxRunning is how many VMs may run at once. Apple's license allows two
	// macOS guests per Mac, which Virtualization.framework also enforces.
	MaxRunning int `json:"maxRunning"`
	Running    int `json:"running"`
	VMs        int `json:"vms"`
}

// Errors with fixed HTTP statuses on the agent API.
var (
	ErrNotFound = errors.New("vm not found")
	ErrExists   = errors.New("a vm with this name already exists on the host")
	ErrFull     = errors.New("the Mac host already runs its maximum number of VMs")
	ErrBusy     = errors.New("the vm is busy; try again in a moment")
)
