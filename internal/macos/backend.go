package macos

import (
	"context"
	"io"
)

// Backend runs the actual VMs for an agent.
type Backend interface {
	// Name is "tart" or "simulator".
	Name() string
	// DefaultMaxRunning is the concurrent VM limit when none is configured.
	DefaultMaxRunning() int
	// Prepare creates the local VM from its image and applies resources.
	// It can take a long time: the first pull of a macOS image is tens of GB.
	Prepare(ctx context.Context, r *record) error
	// Start boots the VM and returns once it has a network address.
	Start(ctx context.Context, r *record) (*Running, error)
	// Attach adopts a VM that is already running (after an agent restart).
	// It returns nil, nil when the VM is not running.
	Attach(ctx context.Context, r *record) (*Running, error)
	Stop(ctx context.Context, r *record) error
	// Delete removes the VM and its disk. Deleting a missing VM is not an error.
	Delete(ctx context.Context, r *record) error
	// Exec runs a shell command in the guest and returns its output.
	Exec(ctx context.Context, r *record, cmd string) (string, error)
	// Shell opens an interactive terminal session in the guest.
	Shell(ctx context.Context, r *record, cols, rows int) (Session, error)
}

// Running describes a booted VM.
type Running struct {
	IP string
	// Targets are the dial addresses for guest ports, as reachable from the
	// agent (the guest IP on the Mac's vmnet bridge for Tart).
	Targets map[int32]string
	// VNC is the dial address of the VM screen, empty without one.
	VNC         string
	VNCPassword string
	// Done is closed when the VM stops for any reason.
	Done <-chan struct{}
}

// Session is an interactive terminal in the guest.
type Session interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
}

// tartName is the local VM name. The prefix keeps the agent away from VMs
// the Mac's owner created by hand.
func tartName(name string) string { return "vk-" + name }
