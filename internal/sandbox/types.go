// Package sandbox turns sandbox requests into Kubernetes objects and reads
// them back. Kubernetes is the only store: every sandbox is a Deployment (a
// container) or a KubeVirt VirtualMachine (a VM) plus owned children, tied
// together by labels and annotations.
package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Labels and annotations on every object the manager creates.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	ManagedByValue = "vishwakarma"
	LabelSandbox   = "vishwakarma.io/sandbox"
	LabelKind      = "vishwakarma.io/kind"
	LabelOwner     = "vishwakarma.io/owner"

	AnnOwner      = "vishwakarma.io/owner"
	AnnExpiresAt  = "vishwakarma.io/expires-at"
	AnnTemplate   = "vishwakarma.io/template"
	AnnImage      = "vishwakarma.io/image"
	AnnPrivileged = "vishwakarma.io/privileged"
	AnnShell      = "vishwakarma.io/shell"
	AnnUser       = "vishwakarma.io/user"
	AnnDisk       = "vishwakarma.io/disk"
	// AnnScreenPort is the VNC port of a container with a screen.
	AnnScreenPort = "vishwakarma.io/screen-port"
)

// Statuses reported for a sandbox.
const (
	StatusPending     = "Pending"
	StatusRunning     = "Running"
	StatusStopped     = "Stopped"
	StatusFailed      = "Failed"
	StatusTerminating = "Terminating"
)

// Expose modes for sandbox ports.
const (
	ExposeCluster  = "cluster"
	ExposeNodePort = "nodeport"
	// ExposeHost is a macOS VM: ports are forwarded on the Mac host.
	ExposeHost = "host"
)

// ContainerName is the name of the sandbox container in its pod.
const ContainerName = "sandbox"

// ScreenContainerName is the Android screen sidecar.
const ScreenContainerName = "screen"

// KubeVirt resources, used through the dynamic client so the server does not
// depend on the KubeVirt Go module.
var (
	VMGVR  = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}
	VMIGVR = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}
)

// Spec is a request to create a sandbox.
type Spec struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Template string `json:"template,omitempty"`
	// Image replaces the template image, or is the whole definition when no
	// template is given. Needs allowCustomImages.
	Image   string            `json:"image,omitempty"`
	Command []string          `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Ports   []int32           `json:"ports,omitempty"`
	Expose  string            `json:"expose,omitempty"`
	CPU     string            `json:"cpu,omitempty"`
	Memory  string            `json:"memory,omitempty"`
	// Disk adds a persistent volume at /data (containers only).
	Disk       string `json:"disk,omitempty"`
	TTL        string `json:"ttl,omitempty"`
	Privileged bool   `json:"privileged,omitempty"`
	// SSHKey is authorized for the VM login user.
	SSHKey string `json:"sshKey,omitempty"`
}

// Sandbox is the view of a sandbox returned by the API.
type Sandbox struct {
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`
	Template   string    `json:"template,omitempty"`
	Image      string    `json:"image"`
	Owner      string    `json:"owner"`
	Status     string    `json:"status"`
	Message    string    `json:"message,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	CPU        string    `json:"cpu"`
	Memory     string    `json:"memory"`
	Disk       string    `json:"disk,omitempty"`
	Privileged bool      `json:"privileged,omitempty"`
	User       string    `json:"user,omitempty"`
	Node       string    `json:"node,omitempty"`
	IP         string    `json:"ip,omitempty"`
	Expose     string    `json:"expose,omitempty"`
	// Host is the Mac a macOS VM runs on.
	Host string `json:"host,omitempty"`
	// Simulated marks a macOS VM from the agent simulator (no real guest).
	Simulated bool `json:"simulated,omitempty"`
	// Screen is set when the console can show the sandbox display (VNC).
	Screen    bool       `json:"screen,omitempty"`
	Endpoints []Endpoint `json:"endpoints"`
}

// Endpoint is one exposed port.
type Endpoint struct {
	// Name labels well-known ports: ssh, vnc (macOS VMs).
	Name     string `json:"name,omitempty"`
	Port     int32  `json:"port"`
	NodePort int32  `json:"nodePort,omitempty"`
	Protocol string `json:"protocol"`
	// Address is how to reach the port: host:nodePort for NodePort services,
	// the in-cluster DNS name otherwise.
	Address string `json:"address"`
}

// Credentials are the login details of a VM.
type Credentials struct {
	User     string `json:"user"`
	Password string `json:"password,omitempty"`
	// VNCPassword opens the screen of a macOS VM.
	VNCPassword string `json:"vncPassword,omitempty"`
}

// Caller is the user a request acts for.
type Caller struct {
	Name  string
	Admin bool
}

// Errors the API maps to HTTP statuses.
var (
	ErrNotFound         = errors.New("sandbox not found")
	ErrExists           = errors.New("a sandbox with this name already exists")
	ErrUnavailable      = errors.New("virtual machines are not available: KubeVirt is not installed or vm.enabled is false")
	ErrMacOSUnavailable = errors.New("macOS is not available: no Mac host agent is configured or reachable")
)

// InvalidError is a request the policy rejects.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &InvalidError{Msg: fmt.Sprintf(format, a...)} }

// QuotaError means the caller has too many sandboxes.
type QuotaError struct{ Max int }

func (e *QuotaError) Error() string {
	return fmt.Sprintf("you already have %d sandboxes, the maximum; delete one first", e.Max)
}

// OwnerLabel is the label value for an owner. Labels cannot hold arbitrary
// user names (emails, spaces), so this is a short hash; the readable name is
// kept in an annotation.
func OwnerLabel(owner string) string {
	sum := sha256.Sum256([]byte(owner))
	return hex.EncodeToString(sum[:])[:20]
}
