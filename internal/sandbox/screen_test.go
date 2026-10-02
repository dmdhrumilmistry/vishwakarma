package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dmdhrumilmistry/vishwakarma/internal/config"
)

func TestAndroidScreenSidecar(t *testing.T) {
	f := newFixture(t, func(p *config.Policy) {
		p.AllowPrivilegedTemplates = true
		p.AndroidScreenImage = "example.test/android-screen:1"
		p.ServerNamespace = "vishwakarma"
	})
	ctx := context.Background()
	sb, err := f.m.Create(ctx, Spec{Name: "droid", Template: "android-12"}, alice)
	if err != nil {
		t.Fatal(err)
	}
	if !sb.Screen {
		t.Error("android sandboxes have a screen")
	}
	d, _ := f.kube.AppsV1().Deployments(ns).Get(ctx, "droid", metav1.GetOptions{})
	cs := d.Spec.Template.Spec.Containers
	if len(cs) != 2 || cs[1].Name != ScreenContainerName || cs[1].Image != "example.test/android-screen:1" {
		t.Fatalf("containers %+v: want the sandbox plus the screen sidecar", cs)
	}
	if sc := cs[1].SecurityContext; sc.Privileged != nil || *sc.AllowPrivilegeEscalation {
		t.Error("the screen sidecar must not be privileged")
	}
	if d.Annotations[AnnScreenPort] != "5900" {
		t.Errorf("screen port annotation %q", d.Annotations[AnnScreenPort])
	}

	// The screen port is reachable from the server namespace only, and is
	// not exposed in the Service.
	np, _ := f.kube.NetworkingV1().NetworkPolicies(ns).Get(ctx, "droid", metav1.GetOptions{})
	var screenRule bool
	for _, r := range np.Spec.Ingress {
		if len(r.Ports) == 1 && r.Ports[0].Port.IntValue() == 5900 {
			screenRule = len(r.From) == 1 && r.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == "vishwakarma"
		}
	}
	if !screenRule {
		t.Errorf("ingress %+v: want 5900 from the server namespace only", np.Spec.Ingress)
	}
	svc, _ := f.kube.CoreV1().Services(ns).Get(ctx, "droid", metav1.GetOptions{})
	for _, p := range svc.Spec.Ports {
		if p.Port == 5900 {
			t.Error("the screen port must not be exposed in the Service")
		}
	}

	// Screen address comes from the running pod.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "droid-1", Namespace: ns, Labels: d.Spec.Template.Labels},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.42.0.9"},
	}
	if _, err := f.kube.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	got, _ := f.m.Get(ctx, "droid", alice)
	addr, err := f.m.ScreenAddr(ctx, got)
	if err != nil || addr != "10.42.0.9:5900" {
		t.Errorf("ScreenAddr = %q, %v", addr, err)
	}
}

func TestMacOSOnLinux(t *testing.T) {
	ctx := context.Background()
	off := newFixture(t, nil)
	var inv *InvalidError
	if _, err := off.m.Create(ctx, Spec{Name: "osx", Template: "macos-linux"}, alice); !errors.As(err, &inv) || !strings.Contains(err.Error(), "non-Apple") {
		t.Fatalf("disabled by default: %v", err)
	}

	on := newFixture(t, func(p *config.Policy) { p.MacOSOnLinux = true })
	sb, err := on.m.Create(ctx, Spec{Name: "osx", Template: "macos-linux"}, alice)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := on.kube.AppsV1().Deployments(ns).Get(ctx, "osx", metav1.GetOptions{})
	c := d.Spec.Template.Spec.Containers[0]
	kvm := corev1.ResourceName("devices.kubevirt.io/kvm")
	if q := c.Resources.Limits[kvm]; q.Value() != 1 {
		t.Errorf("kvm limit %v", q.String())
	}
	if q := c.Resources.Requests[kvm]; q.Value() != 1 {
		t.Errorf("kvm request %v: device resources need requests equal to limits", q.String())
	}
	if c.SecurityContext.Privileged != nil {
		t.Error("Docker-OSX gets /dev/kvm from the device plugin, not from privileged")
	}
	if !sb.Screen || d.Annotations[AnnScreenPort] != "5999" {
		t.Errorf("screen %v %q", sb.Screen, d.Annotations[AnnScreenPort])
	}
	cr, err := on.m.Credentials(ctx, "osx", alice)
	if err != nil || cr.User != "user" || cr.Password != "alpine" {
		t.Errorf("template login %+v %v", cr, err)
	}

	// A custom image keeps neither the template's devices nor its screen.
	if _, err := on.m.Create(ctx, Spec{Name: "other", Template: "macos-linux", Image: "nginx"}, alice); err != nil {
		t.Fatal(err)
	}
	d2, _ := on.kube.AppsV1().Deployments(ns).Get(ctx, "other", metav1.GetOptions{})
	if _, ok := d2.Spec.Template.Spec.Containers[0].Resources.Limits[kvm]; ok {
		t.Error("custom image inherited /dev/kvm")
	}
	if _, err := on.m.Credentials(ctx, "other", alice); err == nil {
		t.Error("custom image inherited the template login")
	}
}
