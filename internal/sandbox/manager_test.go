package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/dmdhrumilmistry/vishwakarma/internal/config"
)

const ns = "sandboxes"

var (
	alice = Caller{Name: "alice@example.com"}
	bob   = Caller{Name: "bob"}
	root  = Caller{Name: "admin", Admin: true}
)

type fixture struct {
	m    *Manager
	kube *fake.Clientset
	dyn  *dynfake.FakeDynamicClient
	now  time.Time
}

func newFixture(t *testing.T, mutate func(*config.Policy)) *fixture {
	t.Helper()
	pol := config.DefaultPolicy()
	pol.Namespace = ns
	pol.PublicHost = "10.0.0.5"
	pol.Templates = config.DefaultTemplates()
	if mutate != nil {
		mutate(pol)
	}
	if err := pol.Validate(); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientset()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		VMGVR:  "VirtualMachineList",
		VMIGVR: "VirtualMachineInstanceList",
	})
	f := &fixture{kube: kube, dyn: dyn, now: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)}
	f.m = NewManager(kube, dyn, pol, func(context.Context) bool { return true }, nil)
	f.m.now = func() time.Time { return f.now }
	return f
}

func TestCreateContainerFromTemplate(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	sb, err := f.m.Create(ctx, Spec{Name: "web", Template: "ubuntu-24.04", Ports: []int32{8080, 80, 80}, Expose: ExposeNodePort, TTL: "2h", Env: map[string]string{"MODE": "test"}}, alice)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Owner != alice.Name || sb.Kind != config.KindContainer || sb.Image != "ubuntu:24.04" {
		t.Fatalf("unexpected view: %+v", sb)
	}
	if want := f.now.Add(2 * time.Hour); !sb.ExpiresAt.Equal(want) {
		t.Errorf("expires %v, want %v", sb.ExpiresAt, want)
	}

	d, err := f.kube.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c := d.Spec.Template.Spec.Containers[0]
	if strings.Join(c.Command, " ") != "sleep infinity" {
		t.Errorf("command %v, want the template sleep", c.Command)
	}
	if c.Resources.Limits.Cpu().String() != "1" || c.Resources.Limits.Memory().String() != "1Gi" {
		t.Errorf("limits %v", c.Resources.Limits)
	}
	if c.Resources.Requests.Cpu().MilliValue() != 250 {
		t.Errorf("cpu request %v, want 250m", c.Resources.Requests.Cpu())
	}
	if c.SecurityContext.Privileged != nil {
		t.Error("container must not be privileged")
	}
	if *d.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Error("sandbox pods must not get a service account token")
	}
	if len(c.Env) != 1 || c.Env[0].Name != "MODE" {
		t.Errorf("env %v", c.Env)
	}
	if d.Labels[LabelOwner] != OwnerLabel(alice.Name) || d.Annotations[AnnOwner] != alice.Name {
		t.Errorf("owner labels %v %v", d.Labels, d.Annotations)
	}

	svc, err := f.kube.CoreV1().Services(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if svc.Spec.Type != corev1.ServiceTypeNodePort || len(svc.Spec.Ports) != 2 || svc.Spec.Ports[0].Port != 80 {
		t.Errorf("service %+v: want NodePort with deduplicated, sorted ports", svc.Spec)
	}
	if len(svc.OwnerReferences) != 1 || svc.OwnerReferences[0].Kind != "Deployment" {
		t.Errorf("service must be owned by the deployment: %v", svc.OwnerReferences)
	}

	np, err := f.kube.NetworkingV1().NetworkPolicies(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(np.Spec.Ingress) != 1 || len(np.Spec.Ingress[0].Ports) != 2 {
		t.Errorf("ingress %+v: want only the exposed ports", np.Spec.Ingress)
	}
	if len(np.Spec.Egress) != 2 || np.Spec.Egress[1].To[0].IPBlock == nil || len(np.Spec.Egress[1].To[0].IPBlock.Except) != 2 {
		t.Errorf("egress %+v: want DNS plus internet minus cluster CIDRs", np.Spec.Egress)
	}
}

func TestCreateCustomImageWithDisk(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	_, err := f.m.Create(ctx, Spec{Name: "app", Image: "nginx:1.29", Disk: "5Gi", CPU: "500m", Memory: "256Mi"}, alice)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := f.kube.AppsV1().Deployments(ns).Get(ctx, "app", metav1.GetOptions{})
	c := d.Spec.Template.Spec.Containers[0]
	if c.Image != "nginx:1.29" || c.Command != nil {
		t.Errorf("custom image must keep its own entrypoint: %v %v", c.Image, c.Command)
	}
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != "/data" {
		t.Errorf("mounts %v", c.VolumeMounts)
	}
	pvc, err := f.kube.CoreV1().PersistentVolumeClaims(ns).Get(ctx, "app-data", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; q.String() != "5Gi" {
		t.Errorf("pvc size %v", q.String())
	}
	if _, err := f.kube.CoreV1().Services(ns).Get(ctx, "app", metav1.GetOptions{}); err == nil {
		t.Error("no ports means no service")
	}
}

func TestPolicyRejections(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*config.Policy)
		spec   Spec
		want   string
	}{
		{"bad name", nil, Spec{Name: "Bad_Name", Template: "alpine"}, "name must be"},
		{"unknown template", nil, Spec{Name: "a", Template: "nope"}, "unknown template"},
		{"kind mismatch", nil, Spec{Name: "a", Template: "alpine", Kind: config.KindVM}, "makes a container"},
		{"nothing to run", nil, Spec{Name: "a"}, "pick a template"},
		{"custom images off", func(p *config.Policy) { p.AllowCustomImages = false }, Spec{Name: "a", Image: "nginx"}, "custom images"},
		{"custom command off", func(p *config.Policy) { p.AllowCustomImages = false }, Spec{Name: "a", Template: "alpine", Command: []string{"sh"}}, "custom images"},
		{"privileged off", nil, Spec{Name: "a", Template: "alpine", Privileged: true}, "privileged"},
		{"privileged template off", func(p *config.Policy) {
			p.Templates = append(p.Templates, config.Template{Name: "agent", Kind: config.KindContainer, Image: "x", Privileged: true})
		}, Spec{Name: "a", Template: "agent"}, "privileged"},
		{"cpu over limit", nil, Spec{Name: "a", Template: "alpine", CPU: "8"}, "above the limit"},
		{"memory over limit", nil, Spec{Name: "a", Template: "alpine", Memory: "64Gi"}, "above the limit"},
		{"bad quantity", nil, Spec{Name: "a", Template: "alpine", Memory: "lots"}, "not a quantity"},
		{"ttl too long", nil, Spec{Name: "a", Template: "alpine", TTL: "1000h"}, "ttl must be"},
		{"ttl garbage", nil, Spec{Name: "a", Template: "alpine", TTL: "soon"}, "not a duration"},
		{"port range", nil, Spec{Name: "a", Template: "alpine", Ports: []int32{70000}}, "out of range"},
		{"nodeport off", func(p *config.Policy) { p.AllowNodePort = false }, Spec{Name: "a", Template: "alpine", Expose: ExposeNodePort}, "NodePort"},
		{"bad env", nil, Spec{Name: "a", Template: "alpine", Env: map[string]string{"1X": "y"}}, "environment variable"},
		{"image with spaces", nil, Spec{Name: "a", Image: "nginx latest"}, "not a valid image"},
		{"disk on vm", nil, Spec{Name: "a", Template: "cirros-vm", Disk: "1Gi"}, "containers only"},
		{"ssh key on container", nil, Spec{Name: "a", Template: "alpine", SSHKey: "ssh-ed25519 AAAA"}, "VMs only"},
		{"bad ssh key", nil, Spec{Name: "a", Template: "cirros-vm", SSHKey: "ssh-ed25519 AAAA\nruncmd: [reboot]"}, "sshKey"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.mutate)
			_, err := f.m.Create(context.Background(), tc.spec, alice)
			var inv *InvalidError
			if !errors.As(err, &inv) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an InvalidError containing %q", err, tc.want)
			}
		})
	}
}

func TestVMsUnavailable(t *testing.T) {
	f := newFixture(t, func(p *config.Policy) { p.VM.Enabled = "false" })
	_, err := f.m.Create(context.Background(), Spec{Name: "v", Template: "cirros-vm"}, alice)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v, want ErrUnavailable", err)
	}
}

func TestQuotaAndConflict(t *testing.T) {
	f := newFixture(t, func(p *config.Policy) { p.MaxSandboxesPerUser = 2 })
	ctx := context.Background()
	for _, n := range []string{"a", "b"} {
		if _, err := f.m.Create(ctx, Spec{Name: n, Template: "alpine"}, alice); err != nil {
			t.Fatal(err)
		}
	}
	var q *QuotaError
	if _, err := f.m.Create(ctx, Spec{Name: "c", Template: "alpine"}, alice); !errors.As(err, &q) {
		t.Fatalf("third sandbox: got %v, want QuotaError", err)
	}
	// Quotas are per user, and admins are exempt.
	if _, err := f.m.Create(ctx, Spec{Name: "c", Template: "alpine"}, bob); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Create(ctx, Spec{Name: "d", Template: "alpine"}, root); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Create(ctx, Spec{Name: "a", Template: "alpine"}, bob); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate name: got %v, want ErrExists", err)
	}
}

func TestOwnersAreIsolated(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	if _, err := f.m.Create(ctx, Spec{Name: "alices", Template: "alpine"}, alice); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Create(ctx, Spec{Name: "bobs", Template: "alpine"}, bob); err != nil {
		t.Fatal(err)
	}

	list, err := f.m.List(ctx, alice)
	if err != nil || len(list) != 1 || list[0].Name != "alices" {
		t.Fatalf("alice sees %v (%v), want only her sandbox", names(list), err)
	}
	if _, err := f.m.Get(ctx, "bobs", alice); !errors.Is(err, ErrNotFound) {
		t.Errorf("alice reading bob's sandbox: %v, want ErrNotFound", err)
	}
	if err := f.m.Delete(ctx, "bobs", alice); !errors.Is(err, ErrNotFound) {
		t.Errorf("alice deleting bob's sandbox: %v, want ErrNotFound", err)
	}
	if _, err := f.m.SetRunning(ctx, "bobs", false, alice); !errors.Is(err, ErrNotFound) {
		t.Errorf("alice stopping bob's sandbox: %v, want ErrNotFound", err)
	}
	if _, err := f.m.Extend(ctx, "bobs", "1h", alice); !errors.Is(err, ErrNotFound) {
		t.Errorf("alice extending bob's sandbox: %v, want ErrNotFound", err)
	}

	all, err := f.m.List(ctx, root)
	if err != nil || len(all) != 2 {
		t.Fatalf("admin sees %v (%v), want both", names(all), err)
	}
	if err := f.m.Delete(ctx, "bobs", root); err != nil {
		t.Fatalf("admin delete: %v", err)
	}
}

func TestLifecycle(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	if _, err := f.m.Create(ctx, Spec{Name: "box", Template: "alpine", TTL: "1h"}, alice); err != nil {
		t.Fatal(err)
	}
	sb, err := f.m.SetRunning(ctx, "box", false, alice)
	if err != nil || sb.Status != StatusStopped {
		t.Fatalf("stop: %v %v", sb, err)
	}
	if sb, err = f.m.SetRunning(ctx, "box", true, alice); err != nil || sb.Status == StatusStopped {
		t.Fatalf("start: %v %v", sb, err)
	}

	f.now = f.now.Add(30 * time.Minute)
	sb, err = f.m.Extend(ctx, "box", "3h", alice)
	if err != nil {
		t.Fatal(err)
	}
	if want := f.now.Add(3 * time.Hour); !sb.ExpiresAt.Equal(want) {
		t.Errorf("extended to %v, want %v", sb.ExpiresAt, want)
	}
	if _, err := f.m.Extend(ctx, "box", "500h", alice); err == nil {
		t.Error("extend past maxTTL must fail")
	}

	if err := f.m.Delete(ctx, "box", alice); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Get(ctx, "box", alice); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
}

func TestReap(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	if _, err := f.m.Create(ctx, Spec{Name: "short", Template: "alpine", TTL: "10m"}, alice); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Create(ctx, Spec{Name: "long", Template: "alpine", TTL: "5h"}, alice); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Create(ctx, Spec{Name: "vm", Template: "cirros-vm", TTL: "10m"}, alice); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.m.Reap(ctx); n != 0 {
		t.Fatalf("reaped %d before anything expired", n)
	}
	f.now = f.now.Add(time.Hour)
	n, err := f.m.Reap(ctx)
	if err != nil || n != 2 {
		t.Fatalf("reaped %d (%v), want 2", n, err)
	}
	list, _ := f.m.List(ctx, alice)
	if len(list) != 1 || list[0].Name != "long" {
		t.Fatalf("left %v, want [long]", names(list))
	}
}

func TestCreateVM(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample you@laptop"
	sb, err := f.m.Create(ctx, Spec{Name: "vm1", Template: "ubuntu-24.04-vm", CPU: "1500m", SSHKey: key, Expose: ExposeNodePort}, alice)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Kind != config.KindVM || sb.User != "ubuntu" || sb.Memory != "2Gi" || sb.CPU != "2" {
		t.Fatalf("view %+v", sb)
	}

	vm, err := f.dyn.Resource(VMGVR).Namespace(ns).Get(ctx, "vm1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	vols, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	if got := vols[0].(map[string]any)["containerDisk"].(map[string]any)["image"]; got != "quay.io/containerdisks/ubuntu:24.04" {
		t.Errorf("root disk %v", got)
	}
	podLabels, _, _ := unstructured.NestedStringMap(vm.Object, "spec", "template", "metadata", "labels")
	if podLabels[LabelSandbox] != "vm1" || podLabels[LabelKind] != config.KindVM {
		t.Errorf("virt-launcher pods need the selector labels: %v", podLabels)
	}

	sec, err := f.kube.CoreV1().Secrets(ns).Get(ctx, "vm1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ud := sec.StringData["userdata"]
	if !strings.HasPrefix(ud, "#cloud-config\n") {
		t.Fatalf("userdata %q", ud)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(ud), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["user"] != "ubuntu" || doc["password"] != sec.StringData["password"] || len(sec.StringData["password"]) != 16 {
		t.Errorf("cloud-init login %v", doc)
	}
	if keys, _ := doc["ssh_authorized_keys"].([]any); len(keys) != 1 || keys[0] != key {
		t.Errorf("ssh keys %v", doc["ssh_authorized_keys"])
	}

	// VMs expose SSH by default.
	svc, err := f.kube.CoreV1().Services(ns).Get(ctx, "vm1", metav1.GetOptions{})
	if err != nil || svc.Spec.Ports[0].Port != 22 {
		t.Fatalf("ssh service %v %v", svc, err)
	}
	if svc.OwnerReferences[0].Kind != "VirtualMachine" {
		t.Errorf("owner %v", svc.OwnerReferences)
	}

	if _, err := f.m.SetRunning(ctx, "vm1", false, alice); err != nil {
		t.Fatal(err)
	}
	vm, _ = f.dyn.Resource(VMGVR).Namespace(ns).Get(ctx, "vm1", metav1.GetOptions{})
	if s, _, _ := unstructured.NestedString(vm.Object, "spec", "runStrategy"); s != "Halted" {
		t.Errorf("runStrategy %q after stop", s)
	}
}

func TestEndpointsAddress(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	if _, err := f.m.Create(ctx, Spec{Name: "np", Template: "alpine", Ports: []int32{80}, Expose: ExposeNodePort}, alice); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Create(ctx, Spec{Name: "cl", Template: "alpine", Ports: []int32{80}}, alice); err != nil {
		t.Fatal(err)
	}
	// The fake API server does not allocate node ports; do it here.
	svc, _ := f.kube.CoreV1().Services(ns).Get(ctx, "np", metav1.GetOptions{})
	svc.Spec.Ports[0].NodePort = 31080
	if _, err := f.kube.CoreV1().Services(ns).Update(ctx, svc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	np, _ := f.m.Get(ctx, "np", alice)
	if np.Endpoints[0].Address != "10.0.0.5:31080" || np.Expose != ExposeNodePort {
		t.Errorf("nodeport endpoint %+v", np.Endpoints)
	}
	cl, _ := f.m.Get(ctx, "cl", alice)
	if cl.Endpoints[0].Address != "cl.sandboxes.svc:80" {
		t.Errorf("cluster endpoint %+v", cl.Endpoints)
	}
}

func TestPodStatus(t *testing.T) {
	mk := func(phase corev1.PodPhase, waiting string) corev1.Pod {
		p := corev1.Pod{Status: corev1.PodStatus{Phase: phase}}
		if waiting != "" {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waiting, Message: "boom"}}}}
		}
		return p
	}
	cases := []struct {
		pods []corev1.Pod
		want string
	}{
		{nil, StatusPending},
		{[]corev1.Pod{mk(corev1.PodRunning, "")}, StatusRunning},
		{[]corev1.Pod{mk(corev1.PodPending, "ContainerCreating")}, StatusPending},
		{[]corev1.Pod{mk(corev1.PodPending, "ImagePullBackOff")}, StatusFailed},
		{[]corev1.Pod{mk(corev1.PodRunning, "CrashLoopBackOff")}, StatusFailed},
	}
	for _, tc := range cases {
		if got, _ := podStatus(tc.pods); got != tc.want {
			t.Errorf("podStatus(%v) = %s, want %s", tc.pods, got, tc.want)
		}
	}
}

func TestVMStatus(t *testing.T) {
	cases := map[string]string{
		"Running":            StatusRunning,
		"Stopped":            StatusStopped,
		"Starting":           StatusPending,
		"ErrImagePull":       StatusFailed,
		"ImagePullBackOff":   StatusFailed,
		"ErrorUnschedulable": StatusFailed,
		"Terminating":        StatusTerminating,
	}
	for printable, want := range cases {
		vm := &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "v"},
			"spec":     map[string]any{"runStrategy": "Always"},
			"status":   map[string]any{"printableStatus": printable},
		}}
		if got := fromVM(vm, nil).Status; got != want {
			t.Errorf("printableStatus %s: got %s, want %s", printable, got, want)
		}
	}
}

func names(list []Sandbox) []string {
	var out []string
	for _, s := range list {
		out = append(out, s.Name)
	}
	return out
}
