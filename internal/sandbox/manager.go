package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/dmdhrumilmistry/vishwakarma/internal/config"
	"github.com/dmdhrumilmistry/vishwakarma/internal/macos"
)

// Manager creates and manages sandboxes in one namespace.
type Manager struct {
	kube   kubernetes.Interface
	dyn    dynamic.Interface
	policy *config.Policy
	log    *slog.Logger
	now    func() time.Time

	// vmProbe reports whether the KubeVirt API is served. Cached briefly
	// because it is asked on every page load.
	vmProbe   func(context.Context) bool
	vmMu      sync.Mutex
	vmCached  bool
	vmChecked time.Time

	// mac runs macOS sandboxes on Mac host agents; nil when not configured.
	mac        *macos.Pool
	macMu      sync.Mutex
	macCached  bool
	macChecked time.Time
}

// NewManager returns a Manager. vmProbe may be nil when VMs are disabled.
func NewManager(kube kubernetes.Interface, dyn dynamic.Interface, policy *config.Policy, vmProbe func(context.Context) bool, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{kube: kube, dyn: dyn, policy: policy, vmProbe: vmProbe, log: log, now: time.Now}
}

// Policy returns the policy the manager enforces.
func (m *Manager) Policy() *config.Policy { return m.policy }

// Namespace is where sandboxes live.
func (m *Manager) Namespace() string { return m.policy.Namespace }

// VMsAvailable reports whether VM sandboxes can be created.
func (m *Manager) VMsAvailable(ctx context.Context) bool {
	switch m.policy.VM.Enabled {
	case "false":
		return false
	case "true":
		return true
	}
	if m.vmProbe == nil {
		return false
	}
	m.vmMu.Lock()
	defer m.vmMu.Unlock()
	if m.now().Sub(m.vmChecked) > 30*time.Second {
		m.vmCached = m.vmProbe(ctx)
		m.vmChecked = m.now()
	}
	return m.vmCached
}

var (
	envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	sshKey  = regexp.MustCompile(`^(ssh-(rsa|ed25519|dss)|ecdsa-sha2-nistp(256|384|521)|sk-(ssh-ed25519|ecdsa-sha2-nistp256)@openssh\.com) [A-Za-z0-9+/=]+( [^\r\n]*)?$`)
)

// resolve validates a Spec against the policy and fills in defaults.
func (m *Manager) resolve(ctx context.Context, s Spec, caller Caller) (*plan, error) {
	pol := m.policy
	p := &plan{Name: strings.TrimSpace(s.Name), Owner: caller.Name, Env: s.Env}
	if !config.ValidName(p.Name) {
		return nil, invalid("name must be 1 to 40 lowercase letters, digits or '-', starting and ending with a letter or digit")
	}

	var tpl config.Template
	if s.Template != "" {
		t, ok := pol.Template(s.Template)
		if !ok {
			return nil, invalid("unknown template %q", s.Template)
		}
		tpl = t
		p.Template = t.Name
	}
	p.Kind = s.Kind
	if p.Kind == "" {
		p.Kind = tpl.Kind
	}
	if p.Kind == "" {
		p.Kind = config.KindContainer
	}
	if p.Kind != config.KindContainer && p.Kind != config.KindVM && p.Kind != config.KindMacOS {
		return nil, invalid("kind must be container, vm or macos")
	}
	if tpl.Name != "" && tpl.Kind != p.Kind {
		return nil, invalid("template %q makes a %s, not a %s", tpl.Name, tpl.Kind, p.Kind)
	}
	if p.Kind == config.KindVM && !m.VMsAvailable(ctx) {
		return nil, ErrUnavailable
	}
	if p.Kind == config.KindMacOS && !m.MacOSAvailable(ctx) {
		return nil, ErrMacOSUnavailable
	}
	if p.Kind == config.KindMacOS && (len(s.Command) > 0 || len(s.Args) > 0 || len(s.Env) > 0) {
		return nil, invalid("command, args and env apply to containers only")
	}

	custom := s.Image != "" || len(s.Command) > 0 || len(s.Args) > 0
	if custom && !pol.AllowCustomImages {
		return nil, invalid("custom images and commands are disabled; pick a template")
	}
	if tpl.Name == "" && s.Image == "" {
		return nil, invalid("pick a template or give an image")
	}
	p.Image = tpl.Image
	p.Command, p.Args = tpl.Command, tpl.Args
	if s.Image != "" {
		p.Image = strings.TrimSpace(s.Image)
		// A new image brings its own entrypoint unless one is given.
		p.Command, p.Args = nil, nil
	}
	if len(s.Command) > 0 || len(s.Args) > 0 {
		p.Command, p.Args = s.Command, s.Args
	}
	if strings.ContainsAny(p.Image, " \t\r\n") || len(p.Image) > 512 {
		return nil, invalid("image %q is not a valid image reference", p.Image)
	}
	p.Shell = tpl.Shell
	p.CloudInit = tpl.CloudInit
	if tpl.MacOSOnLinux && !pol.MacOSOnLinux {
		return nil, invalid("template %q runs macOS on non-Apple hardware, which the administrator has not enabled", tpl.Name)
	}
	// A template's screen and devices belong to its own image.
	if !custom || s.Image == "" || s.Image == tpl.Image {
		p.Screen = tpl.Screen
		if len(tpl.ExtraResources) > 0 {
			p.Extra = map[string]resource.Quantity{}
			for k, v := range tpl.ExtraResources {
				p.Extra[k] = resource.MustParse(v)
			}
		}
	}

	env := map[string]string{}
	for k, v := range tpl.Env {
		env[k] = v
	}
	for k, v := range s.Env {
		env[k] = v
	}
	if len(env) > 64 {
		return nil, invalid("at most 64 environment variables")
	}
	for k := range env {
		if !envName.MatchString(k) {
			return nil, invalid("environment variable name %q is not valid", k)
		}
	}
	p.Env = env

	ports := s.Ports
	if ports == nil {
		ports = tpl.Ports
	}
	if ports == nil && p.Kind == config.KindVM {
		ports = []int32{22}
	}
	if p.Kind == config.KindMacOS {
		// SSH (and VNC) are always forwarded; drop duplicates of them.
		var rest []int32
		for _, port := range ports {
			if port != 22 && port != 5900 {
				rest = append(rest, port)
			}
		}
		ports = rest
	}
	if len(ports) > 16 {
		return nil, invalid("at most 16 ports")
	}
	seen := map[int32]bool{}
	for _, port := range ports {
		if port < 1 || port > 65535 {
			return nil, invalid("port %d is out of range", port)
		}
		if !seen[port] {
			p.Ports = append(p.Ports, port)
		}
		seen[port] = true
	}
	sort.Slice(p.Ports, func(i, j int) bool { return p.Ports[i] < p.Ports[j] })

	p.Expose = s.Expose
	if p.Expose == "" {
		p.Expose = ExposeCluster
	}
	switch {
	case p.Kind == config.KindMacOS:
		// Mac hosts forward ports on their own address.
		p.Expose = ExposeHost
	case p.Expose == ExposeCluster:
	case p.Expose == ExposeNodePort:
		if !pol.AllowNodePort {
			return nil, invalid("NodePort exposure is disabled")
		}
	default:
		return nil, invalid("expose must be %s or %s", ExposeCluster, ExposeNodePort)
	}

	if s.Privileged && !pol.AllowPrivileged {
		return nil, invalid("privileged sandboxes are disabled by the administrator")
	}
	if tpl.Privileged && !pol.AllowPrivileged {
		if !pol.AllowPrivilegedTemplates {
			return nil, invalid("template %q needs a privileged container, which the administrator has disabled", tpl.Name)
		}
		if custom {
			return nil, invalid("template %q runs privileged, so it must keep its own image and command", tpl.Name)
		}
	}
	p.Privileged = s.Privileged || tpl.Privileged
	if p.Privileged && p.Kind != config.KindContainer {
		return nil, invalid("privileged applies to containers only; a VM already has its own kernel")
	}

	var err error
	if p.CPU, err = quantity("cpu", s.CPU, tpl.Resources.CPU, pol.Defaults.CPU, pol.Limits.CPU); err != nil {
		return nil, err
	}
	if p.Memory, err = quantity("memory", s.Memory, tpl.Resources.Memory, pol.Defaults.Memory, pol.Limits.Memory); err != nil {
		return nil, err
	}
	if s.Disk != "" || tpl.Resources.Disk != "" {
		if p.Kind == config.KindVM {
			return nil, invalid("disk applies to containers only; a VM root disk comes from its image")
		}
		d, err := quantity("disk", s.Disk, tpl.Resources.Disk, pol.Defaults.Disk, pol.Limits.Disk)
		if err != nil {
			return nil, err
		}
		p.Disk = &d
	}

	ttl := pol.DefaultTTL.Duration
	if s.TTL != "" {
		if ttl, err = time.ParseDuration(s.TTL); err != nil {
			return nil, invalid("ttl %q is not a duration such as 30m or 4h", s.TTL)
		}
	}
	if ttl < time.Minute || ttl > pol.MaxTTL.Duration {
		return nil, invalid("ttl must be between 1m and %s", pol.MaxTTL.Duration)
	}
	p.ExpiresAt = m.now().Add(ttl).Truncate(time.Second)

	switch p.Kind {
	case config.KindVM:
		p.User = tpl.User
		if p.User == "" {
			p.User = "vishwakarma"
		}
		p.Password = randomPassword(16)
	case config.KindMacOS:
		// macOS images ship a fixed login; the agent replaces the password
		// on first boot unless the template says to keep it.
		p.User, p.Password, p.KeepPassword = tpl.User, tpl.Password, tpl.KeepPassword
		if p.User == "" {
			p.User, p.Password = "admin", "admin"
		}
	default:
		if s.SSHKey != "" {
			return nil, invalid("sshKey applies to VMs only")
		}
	}
	p.SSHKey = strings.TrimSpace(s.SSHKey)
	if p.SSHKey != "" && (len(p.SSHKey) > 4096 || !sshKey.MatchString(p.SSHKey)) {
		return nil, invalid("sshKey must be a single OpenSSH public key line")
	}
	return p, nil
}

func quantity(field, req, tpl, def, max string) (resource.Quantity, error) {
	v := req
	if v == "" {
		v = tpl
	}
	if v == "" {
		v = def
	}
	q, err := resource.ParseQuantity(v)
	if err != nil {
		return q, invalid("%s %q is not a quantity such as 500m, 2, 512Mi or 4Gi", field, v)
	}
	if q.Sign() <= 0 {
		return q, invalid("%s must be positive", field)
	}
	if lim := resource.MustParse(max); q.Cmp(lim) > 0 {
		return q, invalid("%s %s is above the limit of %s", field, q.String(), lim.String())
	}
	return q, nil
}

// Create validates the spec and creates the sandbox objects.
func (m *Manager) Create(ctx context.Context, s Spec, caller Caller) (*Sandbox, error) {
	p, err := m.resolve(ctx, s, caller)
	if err != nil {
		return nil, err
	}
	if !caller.Admin && m.policy.MaxSandboxesPerUser > 0 {
		n, err := m.count(ctx, caller.Name)
		if err != nil {
			return nil, err
		}
		if n >= m.policy.MaxSandboxesPerUser {
			return nil, &QuotaError{Max: m.policy.MaxSandboxesPerUser}
		}
	}
	if exists, err := m.exists(ctx, p.Name); err != nil {
		return nil, err
	} else if exists {
		return nil, ErrExists
	}

	if p.Kind == config.KindMacOS {
		if err := m.createMac(ctx, p); err != nil {
			return nil, err
		}
		m.log.Info("sandbox created", "name", p.Name, "kind", p.Kind, "image", p.Image, "owner", p.Owner, "expires", p.ExpiresAt)
		return m.Get(ctx, p.Name, caller)
	}

	ns := m.policy.Namespace
	var ref metav1.OwnerReference
	switch p.Kind {
	case config.KindContainer:
		d, err := m.kube.AppsV1().Deployments(ns).Create(ctx, buildDeployment(p, m.policy), metav1.CreateOptions{})
		if err != nil {
			return nil, conflict(err)
		}
		ref = ownerRef("apps/v1", "Deployment", d.Name, d.UID)
		if p.Disk != nil {
			pvc := buildDataClaim(p, m.policy)
			pvc.OwnerReferences = []metav1.OwnerReference{ref}
			if _, err := m.kube.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
				return nil, m.rollback(ctx, p, err)
			}
		}
	case config.KindVM:
		vm, err := m.dyn.Resource(VMGVR).Namespace(ns).Create(ctx, buildVM(p, m.policy), metav1.CreateOptions{})
		if err != nil {
			return nil, conflict(err)
		}
		ref = ownerRef("kubevirt.io/v1", "VirtualMachine", vm.GetName(), vm.GetUID())
		sec, err := buildVMSecret(p)
		if err != nil {
			return nil, m.rollback(ctx, p, err)
		}
		sec.OwnerReferences = []metav1.OwnerReference{ref}
		if _, err := m.kube.CoreV1().Secrets(ns).Create(ctx, sec, metav1.CreateOptions{}); err != nil {
			return nil, m.rollback(ctx, p, err)
		}
	}

	if svc := buildService(p); svc != nil {
		svc.OwnerReferences = []metav1.OwnerReference{ref}
		if _, err := m.kube.CoreV1().Services(ns).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
			return nil, m.rollback(ctx, p, err)
		}
	}
	if m.policy.Network.Isolate {
		np := buildNetworkPolicy(p, m.policy)
		np.OwnerReferences = []metav1.OwnerReference{ref}
		if _, err := m.kube.NetworkingV1().NetworkPolicies(ns).Create(ctx, np, metav1.CreateOptions{}); err != nil {
			return nil, m.rollback(ctx, p, err)
		}
	}
	m.log.Info("sandbox created", "name", p.Name, "kind", p.Kind, "image", p.Image, "owner", p.Owner, "expires", p.ExpiresAt)
	return m.Get(ctx, p.Name, caller)
}

// rollback removes a half-created sandbox; children go with the owner.
func (m *Manager) rollback(ctx context.Context, p *plan, cause error) error {
	if err := m.deletePrimary(ctx, p.Name, p.Kind); err != nil && !apierrors.IsNotFound(err) {
		m.log.Error("rollback failed", "name", p.Name, "err", err)
	}
	return fmt.Errorf("create %s: %w", p.Name, cause)
}

func conflict(err error) error {
	if apierrors.IsAlreadyExists(err) {
		return ErrExists
	}
	return err
}

func (m *Manager) exists(ctx context.Context, name string) (bool, error) {
	ns := m.policy.Namespace
	if _, err := m.kube.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
		return true, nil
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}
	if m.VMsAvailable(ctx) {
		if _, err := m.dyn.Resource(VMGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
			return true, nil
		} else if !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	if len(m.macList(ctx, labels.Set{LabelSandbox: name})) > 0 {
		return true, nil
	}
	return false, nil
}

func (m *Manager) count(ctx context.Context, owner string) (int, error) {
	list, err := m.list(ctx, labels.Set{LabelOwner: OwnerLabel(owner)})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range list {
		if s.Owner == owner {
			n++
		}
	}
	return n, nil
}

// List returns the caller's sandboxes, or every sandbox for an admin.
func (m *Manager) List(ctx context.Context, caller Caller) ([]Sandbox, error) {
	sel := labels.Set{}
	if !caller.Admin {
		sel[LabelOwner] = OwnerLabel(caller.Name)
	}
	all, err := m.list(ctx, sel)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, s := range all {
		// The owner label is a hash prefix; compare the full name too.
		if caller.Admin || s.Owner == caller.Name {
			out = append(out, s)
		}
	}
	return out, nil
}

// Get returns one sandbox the caller may see.
func (m *Manager) Get(ctx context.Context, name string, caller Caller) (*Sandbox, error) {
	all, err := m.list(ctx, labels.Set{LabelSandbox: name})
	if err != nil {
		return nil, err
	}
	for _, s := range all {
		if s.Name == name && (caller.Admin || s.Owner == caller.Name) {
			return &s, nil
		}
	}
	return nil, ErrNotFound
}

func (m *Manager) list(ctx context.Context, extra labels.Set) ([]Sandbox, error) {
	ns := m.policy.Namespace
	set := labels.Set{LabelManagedBy: ManagedByValue}
	for k, v := range extra {
		set[k] = v
	}
	opts := metav1.ListOptions{LabelSelector: set.String()}

	deps, err := m.kube.AppsV1().Deployments(ns).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	pods, err := m.kube.CoreV1().Pods(ns).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	svcs, err := m.kube.CoreV1().Services(ns).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	podsBy := map[string][]corev1.Pod{}
	for _, pod := range pods.Items {
		podsBy[pod.Labels[LabelSandbox]] = append(podsBy[pod.Labels[LabelSandbox]], pod)
	}
	svcBy := map[string]*corev1.Service{}
	for i := range svcs.Items {
		svcBy[svcs.Items[i].Name] = &svcs.Items[i]
	}

	var out []Sandbox
	for i := range deps.Items {
		d := &deps.Items[i]
		s := fromDeployment(d, podsBy[d.Name])
		s.Expose, s.Endpoints = m.endpoints(svcBy[d.Name])
		out = append(out, s)
	}

	if m.VMsAvailable(ctx) {
		vms, err := m.dyn.Resource(VMGVR).Namespace(ns).List(ctx, opts)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("list virtual machines: %w", err)
		}
		if vms != nil && len(vms.Items) > 0 {
			vmis, err := m.dyn.Resource(VMIGVR).Namespace(ns).List(ctx, opts)
			if err != nil && !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("list virtual machine instances: %w", err)
			}
			vmiBy := map[string]*unstructured.Unstructured{}
			if vmis != nil {
				for i := range vmis.Items {
					vmiBy[vmis.Items[i].GetName()] = &vmis.Items[i]
				}
			}
			for i := range vms.Items {
				vm := &vms.Items[i]
				s := fromVM(vm, vmiBy[vm.GetName()])
				s.Expose, s.Endpoints = m.endpoints(svcBy[vm.GetName()])
				out = append(out, s)
			}
		}
	}
	out = append(out, m.macList(ctx, extra)...)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *Manager) endpoints(svc *corev1.Service) (string, []Endpoint) {
	eps := []Endpoint{}
	if svc == nil {
		return "", eps
	}
	expose := ExposeCluster
	if svc.Spec.Type == corev1.ServiceTypeNodePort {
		expose = ExposeNodePort
	}
	host := m.policy.PublicHost
	if host == "" {
		host = "<node-ip>"
	}
	for _, sp := range svc.Spec.Ports {
		ep := Endpoint{Port: sp.Port, Protocol: string(sp.Protocol)}
		if expose == ExposeNodePort && sp.NodePort != 0 {
			ep.NodePort = sp.NodePort
			ep.Address = fmt.Sprintf("%s:%d", host, sp.NodePort)
		} else {
			ep.Address = fmt.Sprintf("%s.%s.svc:%d", svc.Name, svc.Namespace, sp.Port)
		}
		eps = append(eps, ep)
	}
	return expose, eps
}

// Delete removes a sandbox and everything it owns.
func (m *Manager) Delete(ctx context.Context, name string, caller Caller) error {
	s, err := m.Get(ctx, name, caller)
	if err != nil {
		return err
	}
	if err := m.remove(ctx, s); err != nil {
		return err
	}
	m.log.Info("sandbox deleted", "name", name, "by", caller.Name)
	return nil
}

// remove deletes any kind of sandbox.
func (m *Manager) remove(ctx context.Context, s *Sandbox) error {
	if s.Kind == config.KindMacOS {
		err := macErr(m.mac.Delete(ctx, s.Host, s.Name))
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	if err := m.deletePrimary(ctx, s.Name, s.Kind); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (m *Manager) deletePrimary(ctx context.Context, name, kind string) error {
	bg := metav1.DeletePropagationBackground
	opts := metav1.DeleteOptions{PropagationPolicy: &bg}
	ns := m.policy.Namespace
	if kind == config.KindVM {
		return m.dyn.Resource(VMGVR).Namespace(ns).Delete(ctx, name, opts)
	}
	return m.kube.AppsV1().Deployments(ns).Delete(ctx, name, opts)
}

// SetRunning starts or stops a sandbox. A stopped container keeps its /data
// volume; a stopped VM loses its containerDisk changes.
func (m *Manager) SetRunning(ctx context.Context, name string, running bool, caller Caller) (*Sandbox, error) {
	s, err := m.Get(ctx, name, caller)
	if err != nil {
		return nil, err
	}
	ns := m.policy.Namespace
	if s.Kind == config.KindMacOS {
		if err := macErr(m.mac.SetRunning(ctx, s.Host, name, running)); err != nil {
			return nil, err
		}
	} else if s.Kind == config.KindVM {
		strategy := "Halted"
		if running {
			strategy = "Always"
		}
		patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"runStrategy": strategy}})
		if _, err := m.dyn.Resource(VMGVR).Namespace(ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			return nil, err
		}
	} else {
		replicas := 0
		if running {
			replicas = 1
		}
		patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"replicas": replicas}})
		if _, err := m.kube.AppsV1().Deployments(ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			return nil, err
		}
	}
	m.log.Info("sandbox power", "name", name, "running", running, "by", caller.Name)
	return m.Get(ctx, name, caller)
}

// Extend sets the expiry to now plus ttl.
func (m *Manager) Extend(ctx context.Context, name, ttl string, caller Caller) (*Sandbox, error) {
	d, err := time.ParseDuration(ttl)
	if err != nil {
		return nil, invalid("ttl %q is not a duration such as 30m or 4h", ttl)
	}
	if d < time.Minute || d > m.policy.MaxTTL.Duration {
		return nil, invalid("ttl must be between 1m and %s", m.policy.MaxTTL.Duration)
	}
	s, err := m.Get(ctx, name, caller)
	if err != nil {
		return nil, err
	}
	if s.Kind == config.KindMacOS {
		if err := macErr(m.mac.Extend(ctx, s.Host, name, m.now().Add(d).Truncate(time.Second).UTC())); err != nil {
			return nil, err
		}
		return m.Get(ctx, name, caller)
	}
	expires := m.now().Add(d).Truncate(time.Second).UTC().Format(time.RFC3339)
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{AnnExpiresAt: expires}}})
	ns := m.policy.Namespace
	if s.Kind == config.KindVM {
		_, err = m.dyn.Resource(VMGVR).Namespace(ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	} else {
		_, err = m.kube.AppsV1().Deployments(ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	}
	if err != nil {
		return nil, err
	}
	return m.Get(ctx, name, caller)
}

// Credentials returns the VM login. Containers have none.
func (m *Manager) Credentials(ctx context.Context, name string, caller Caller) (*Credentials, error) {
	s, err := m.Get(ctx, name, caller)
	if err != nil {
		return nil, err
	}
	if s.Kind == config.KindMacOS {
		c, err := m.mac.Credentials(ctx, s.Host, name)
		if err != nil {
			return nil, macErr(err)
		}
		return &Credentials{User: c.User, Password: c.Password, VNCPassword: c.VNCPassword}, nil
	}
	if s.Kind == config.KindContainer {
		// Some images ship a fixed login (Docker-OSX); show the template's.
		if t, ok := m.policy.Template(s.Template); ok && t.User != "" && t.Image == s.Image {
			return &Credentials{User: t.User, Password: t.Password}, nil
		}
		return nil, invalid("containers have no login; use the terminal")
	}
	sec, err := m.kube.CoreV1().Secrets(m.policy.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return &Credentials{User: s.User}, nil
	}
	if err != nil {
		return nil, err
	}
	return &Credentials{User: string(sec.Data["user"]), Password: string(sec.Data["password"])}, nil
}

// ScreenAddr is the VNC address of a container sandbox with a screen.
func (m *Manager) ScreenAddr(ctx context.Context, s *Sandbox) (string, error) {
	if !s.Screen || s.Kind != config.KindContainer {
		return "", invalid("this sandbox has no screen")
	}
	pod, err := m.RunningPod(ctx, s)
	if err != nil {
		return "", err
	}
	d, err := m.kube.AppsV1().Deployments(m.policy.Namespace).Get(ctx, s.Name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	port := d.Annotations[AnnScreenPort]
	if pod.Status.PodIP == "" || port == "" {
		return "", invalid("the sandbox has no screen address yet")
	}
	return net.JoinHostPort(pod.Status.PodIP, port), nil
}

// RunningPod returns the pod to open a terminal or read logs in.
func (m *Manager) RunningPod(ctx context.Context, s *Sandbox) (*corev1.Pod, error) {
	sel := labels.Set{LabelSandbox: s.Name, LabelKind: s.Kind}.String()
	pods, err := m.kube.CoreV1().Pods(m.policy.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
			return p, nil
		}
	}
	return nil, invalid("the sandbox is not running")
}

// Reap deletes expired sandboxes. It returns how many it removed.
func (m *Manager) Reap(ctx context.Context) (int, error) {
	all, err := m.list(ctx, nil)
	if err != nil {
		return 0, err
	}
	n := 0
	now := m.now()
	for _, s := range all {
		if s.ExpiresAt.IsZero() || s.ExpiresAt.After(now) || s.Status == StatusTerminating {
			continue
		}
		if err := m.remove(ctx, &s); err != nil {
			m.log.Error("reap failed", "name", s.Name, "err", err)
			continue
		}
		m.log.Info("sandbox expired and deleted", "name", s.Name, "owner", s.Owner, "expired", s.ExpiresAt)
		n++
	}
	return n, nil
}

// RunReaper reaps every interval until ctx is done.
func (m *Manager) RunReaper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := m.Reap(ctx); err != nil && ctx.Err() == nil {
			m.log.Error("reaper", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// fromDeployment builds the view of a container sandbox.
func fromDeployment(d *appsv1.Deployment, pods []corev1.Pod) Sandbox {
	s := common(d.ObjectMeta)
	s.Kind = config.KindContainer
	if c := d.Spec.Template.Spec.Containers; len(c) > 0 {
		s.CPU = c[0].Resources.Limits.Cpu().String()
		s.Memory = c[0].Resources.Limits.Memory().String()
		if c[0].SecurityContext != nil && c[0].SecurityContext.Privileged != nil {
			s.Privileged = *c[0].SecurityContext.Privileged
		}
	}
	s.Disk = d.Annotations[AnnDisk]
	s.Screen = d.Annotations[AnnScreenPort] != ""

	switch {
	case d.DeletionTimestamp != nil:
		s.Status = StatusTerminating
	case d.Spec.Replicas != nil && *d.Spec.Replicas == 0:
		s.Status = StatusStopped
		if len(pods) > 0 {
			s.Message = "stopping"
		}
	default:
		s.Status, s.Message = podStatus(pods)
	}
	for _, p := range pods {
		if p.Status.Phase == corev1.PodRunning {
			s.Node, s.IP = p.Spec.NodeName, p.Status.PodIP
		}
	}
	return s
}

// podStatus summarises the newest pod of a running container sandbox.
func podStatus(pods []corev1.Pod) (string, string) {
	if len(pods) == 0 {
		return StatusPending, "waiting for a pod"
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].CreationTimestamp.After(pods[j].CreationTimestamp.Time) })
	p := pods[0]
	for _, cs := range p.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CrashLoopBackOff", "CreateContainerConfigError", "CreateContainerError", "RunContainerError":
				return StatusFailed, strings.TrimSpace(w.Reason + ": " + w.Message)
			}
			return StatusPending, w.Reason
		}
		if t := cs.State.Terminated; t != nil {
			return StatusFailed, fmt.Sprintf("exited with code %d: %s", t.ExitCode, t.Reason)
		}
	}
	switch p.Status.Phase {
	case corev1.PodRunning:
		return StatusRunning, ""
	case corev1.PodFailed:
		return StatusFailed, p.Status.Message
	case corev1.PodSucceeded:
		return StatusFailed, "the main process exited"
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return StatusPending, c.Message
		}
	}
	return StatusPending, "starting"
}

// fromVM builds the view of a VM sandbox.
func fromVM(vm, vmi *unstructured.Unstructured) Sandbox {
	meta := metav1.ObjectMeta{
		Name:              vm.GetName(),
		Labels:            vm.GetLabels(),
		Annotations:       vm.GetAnnotations(),
		CreationTimestamp: vm.GetCreationTimestamp(),
		DeletionTimestamp: vm.GetDeletionTimestamp(),
	}
	s := common(meta)
	s.Kind = config.KindVM
	s.User = meta.Annotations[AnnUser]
	cores, _, _ := unstructured.NestedInt64(vm.Object, "spec", "template", "spec", "domain", "cpu", "cores")
	if cores == 0 {
		cores = 1
	}
	s.CPU = fmt.Sprint(cores)
	s.Screen = true // KubeVirt serves a VNC display for every VM
	s.Memory, _, _ = unstructured.NestedString(vm.Object, "spec", "template", "spec", "domain", "memory", "guest")

	printable, _, _ := unstructured.NestedString(vm.Object, "status", "printableStatus")
	strategy, _, _ := unstructured.NestedString(vm.Object, "spec", "runStrategy")
	switch {
	case vm.GetDeletionTimestamp() != nil || printable == "Terminating":
		s.Status = StatusTerminating
	case printable == "Running":
		s.Status = StatusRunning
	case printable == "Stopped" || (strategy == "Halted" && printable == ""):
		s.Status = StatusStopped
	case printable == "Stopping":
		s.Status, s.Message = StatusStopped, "stopping"
	case strings.HasPrefix(printable, "Err") || strings.HasSuffix(printable, "BackOff") || printable == "ErrorUnschedulable" || printable == "DataVolumeError":
		s.Status, s.Message = StatusFailed, printable+vmMessage(vm)
	default:
		s.Status = StatusPending
		s.Message = printable
		if s.Message == "" {
			s.Message = "provisioning"
		}
	}
	if vmi != nil {
		s.Node, _, _ = unstructured.NestedString(vmi.Object, "status", "nodeName")
		ifaces, _, _ := unstructured.NestedSlice(vmi.Object, "status", "interfaces")
		for _, i := range ifaces {
			if m, ok := i.(map[string]any); ok {
				if ip, ok := m["ipAddress"].(string); ok && ip != "" {
					s.IP = ip
					break
				}
			}
		}
	}
	return s
}

func vmMessage(vm *unstructured.Unstructured) string {
	conds, _, _ := unstructured.NestedSlice(vm.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if msg, _ := m["message"].(string); msg != "" && m["status"] == "False" {
			return ": " + msg
		}
	}
	return ""
}

func common(meta metav1.ObjectMeta) Sandbox {
	s := Sandbox{
		Name:      meta.Name,
		Owner:     meta.Annotations[AnnOwner],
		Template:  meta.Annotations[AnnTemplate],
		Image:     meta.Annotations[AnnImage],
		CreatedAt: meta.CreationTimestamp.Time,
		Endpoints: []Endpoint{},
	}
	if t, err := time.Parse(time.RFC3339, meta.Annotations[AnnExpiresAt]); err == nil {
		s.ExpiresAt = t
	}
	return s
}

const passwordAlphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func randomPassword(n int) string {
	b := make([]byte, n)
	max := big.NewInt(int64(len(passwordAlphabet)))
	for i := range b {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		b[i] = passwordAlphabet[v.Int64()]
	}
	return string(b)
}
