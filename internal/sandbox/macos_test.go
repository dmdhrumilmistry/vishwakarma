package sandbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dmdhrumilmistry/vishwakarma/internal/config"
	"github.com/dmdhrumilmistry/vishwakarma/internal/macos"
)

const agentToken = "agent-token-agent-token-agent-token"

// withMac attaches a simulator agent to the fixture's manager.
func withMac(t *testing.T, f *fixture) *macos.Agent {
	t.Helper()
	sim := macos.NewSimulator()
	sim.PrepareDelay = 10 * time.Millisecond
	a, err := macos.NewAgent(macos.AgentConfig{
		Name: "mac1", PublicHost: "mac.lan", Token: agentToken,
		BindAddr: "127.0.0.1", PortMin: 42000, PortMax: 42999,
	}, sim, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		for _, vm := range a.List() {
			_ = a.Delete(vm.Name)
		}
	})
	f.m.SetMacOS(macos.NewPool([]config.MacAgent{{Name: "mac1", URL: srv.URL}}, agentToken))
	return a
}

func waitStatus(t *testing.T, f *fixture, name, want string, c Caller) *Sandbox {
	t.Helper()
	// The agent works in real time, the fixture clock does not move.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s, err := f.m.Get(context.Background(), name, c); err == nil && s.Status == want {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	s, err := f.m.Get(context.Background(), name, c)
	t.Fatalf("%s never became %s: %+v %v", name, want, s, err)
	return nil
}

func TestMacOSSandbox(t *testing.T) {
	f := newFixture(t, nil)
	withMac(t, f)
	ctx := context.Background()

	sb, err := f.m.Create(ctx, Spec{Name: "mac", Template: "macos-sequoia", Ports: []int32{22, 8080}, TTL: "2h"}, alice)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Kind != config.KindMacOS || sb.Host != "mac1" || sb.Memory != "8Gi" || sb.CPU != "4" || sb.User != "admin" {
		t.Fatalf("created %+v", sb)
	}
	run := waitStatus(t, f, "mac", StatusRunning, alice)
	for _, e := range run.Endpoints {
		if !strings.HasPrefix(e.Address, "mac.lan:") {
			t.Errorf("endpoint %+v must use the Mac's public host", e)
		}
	}
	if len(run.Endpoints) != 3 {
		t.Errorf("endpoints %+v: want ssh, vnc and 8080 (22 is not duplicated)", run.Endpoints)
	}

	creds, err := f.m.Credentials(ctx, "mac", alice)
	if err != nil || creds.User != "admin" || creds.Password == "admin" || creds.VNCPassword == "" {
		t.Fatalf("credentials %+v %v", creds, err)
	}

	// Owners are isolated for macOS like for everything else.
	if _, err := f.m.Get(ctx, "mac", bob); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob reads alice's Mac: %v", err)
	}
	if l, _ := f.m.List(ctx, bob); len(l) != 0 {
		t.Errorf("bob lists %v", names(l))
	}
	if l, _ := f.m.List(ctx, root); len(l) != 1 {
		t.Errorf("admin lists %v", names(l))
	}
	// Names are unique across Kubernetes and Macs.
	if _, err := f.m.Create(ctx, Spec{Name: "mac", Template: "alpine"}, alice); !errors.Is(err, ErrExists) {
		t.Errorf("container with a Mac VM's name: %v", err)
	}

	if _, err := f.m.SetRunning(ctx, "mac", false, alice); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, f, "mac", StatusStopped, alice)
	if _, err := f.m.Extend(ctx, "mac", "5h", alice); err != nil {
		t.Fatal(err)
	}

	// Expiry is enforced by the server's reaper.
	f.now = f.now.Add(6 * time.Hour)
	if n, err := f.m.Reap(ctx); err != nil || n != 1 {
		t.Fatalf("reaped %d (%v)", n, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := f.m.Get(ctx, "mac", alice); errors.Is(err, ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reaped Mac VM still listed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMacOSRules(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	if _, err := f.m.Create(ctx, Spec{Name: "m", Template: "macos-sequoia"}, alice); !errors.Is(err, ErrMacOSUnavailable) {
		t.Fatalf("no agents: %v", err)
	}
	withMac(t, f)
	cases := map[string]Spec{
		"env":        {Name: "m", Template: "macos-sequoia", Env: map[string]string{"A": "b"}},
		"command":    {Name: "m", Template: "macos-sequoia", Command: []string{"x"}},
		"privileged": {Name: "m", Template: "macos-sequoia", Privileged: true},
		"too big":    {Name: "m", Template: "macos-sequoia", Memory: "64Gi"},
	}
	for name, s := range cases {
		var inv *InvalidError
		if _, err := f.m.Create(ctx, s, alice); !errors.As(err, &inv) {
			t.Errorf("%s: %v, want InvalidError", name, err)
		}
	}
}

func TestPrivilegedTemplates(t *testing.T) {
	ctx := context.Background()

	off := newFixture(t, nil)
	if _, err := off.m.Create(ctx, Spec{Name: "a", Template: "android-12"}, alice); err == nil || !strings.Contains(err.Error(), "privileged") {
		t.Fatalf("android with privileged templates off: %v", err)
	}

	on := newFixture(t, func(p *config.Policy) { p.AllowPrivilegedTemplates = true })
	if _, err := on.m.Create(ctx, Spec{Name: "droid", Template: "android-12", Disk: "4Gi"}, alice); err != nil {
		t.Fatal(err)
	}
	d, _ := on.kube.AppsV1().Deployments(ns).Get(ctx, "droid", metav1.GetOptions{})
	c := d.Spec.Template.Spec.Containers[0]
	if c.SecurityContext.Privileged == nil || !*c.SecurityContext.Privileged || !c.TTY || !c.Stdin {
		t.Errorf("android container must be privileged with a TTY: %+v", c)
	}
	if c.Image != "redroid/redroid:12.0.0_64only-latest" || c.Args[0] != "androidboot.redroid_gpu_mode=guest" {
		t.Errorf("android image/args %v %v", c.Image, c.Args)
	}
	if c.Resources.Requests.Memory().Cmp(*c.Resources.Limits.Memory()) >= 0 {
		t.Error("memory request must be below the limit so Android fits on small nodes")
	}
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != "/data" {
		t.Errorf("android data volume %v", c.VolumeMounts)
	}
	svc, _ := on.kube.CoreV1().Services(ns).Get(ctx, "droid", metav1.GetOptions{})
	if svc.Spec.Ports[0].Port != 5555 || svc.Spec.Ports[0].Protocol != corev1.ProtocolTCP {
		t.Errorf("adb port %+v", svc.Spec.Ports)
	}

	// The template's privilege must not extend to an image of the user's choosing.
	if _, err := on.m.Create(ctx, Spec{Name: "evil", Template: "android-12", Image: "attacker/rootkit"}, alice); err == nil {
		t.Error("custom image on a privileged template accepted")
	}
	if _, err := on.m.Create(ctx, Spec{Name: "evil2", Template: "alpine", Privileged: true}, alice); err == nil {
		t.Error("user-requested privileged accepted with only privileged templates allowed")
	}
}
