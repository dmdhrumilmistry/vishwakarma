package sandbox

import (
	"math"
	"sort"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"

	"github.com/dmdhrumilmistry/vishwakarma/internal/config"
)

// plan is a validated Spec with every default resolved. It is what the
// builders turn into objects.
type plan struct {
	Name       string
	Kind       string
	Template   string
	Image      string
	Command    []string
	Args       []string
	Env        map[string]string
	Ports      []int32
	Expose     string
	CPU        resource.Quantity
	Memory     resource.Quantity
	Disk       *resource.Quantity
	Privileged bool
	Shell      string
	User       string
	Password   string
	// KeepPassword keeps a macOS image's own password.
	KeepPassword bool
	// Screen serves the display over VNC (containers).
	Screen *config.Screen
	// Extra are added to the container's requests and limits (e.g. /dev/kvm).
	Extra map[string]resource.Quantity
	// Emulated: the template wanted /dev/kvm but no node has it.
	Emulated bool
	// ReserveMemory requests the full memory limit.
	ReserveMemory bool
	SSHKey        string
	CloudInit     string
	Owner         string
	ExpiresAt     time.Time
}

func (p *plan) labels() map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelSandbox:   p.Name,
		LabelKind:      p.Kind,
		LabelOwner:     OwnerLabel(p.Owner),
	}
}

// selector matches the sandbox workload pods (the virt-launcher pod for VMs).
func (p *plan) selector() map[string]string {
	return map[string]string{LabelSandbox: p.Name, LabelKind: p.Kind}
}

func (p *plan) annotations() map[string]string {
	a := map[string]string{
		AnnOwner:     p.Owner,
		AnnExpiresAt: p.ExpiresAt.UTC().Format(time.RFC3339),
		AnnTemplate:  p.Template,
		AnnImage:     p.Image,
	}
	if p.Privileged {
		a[AnnPrivileged] = "true"
	}
	if p.Shell != "" {
		a[AnnShell] = p.Shell
	}
	if p.User != "" {
		a[AnnUser] = p.User
	}
	if p.Disk != nil {
		a[AnnDisk] = p.Disk.String()
	}
	if p.Screen != nil {
		a[AnnScreenPort] = strconv.Itoa(int(p.Screen.Port))
	}
	if p.Emulated {
		a[AnnEmulated] = "true"
	}
	return a
}

func (p *plan) meta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Labels: p.labels(), Annotations: map[string]string{AnnOwner: p.Owner}}
}

func dataClaimName(name string) string { return name + "-data" }

// buildDeployment renders a container sandbox.
func buildDeployment(p *plan, pol *config.Policy) *appsv1.Deployment {
	one := int32(1)
	grace := int64(5)
	no := false

	limits := corev1.ResourceList{corev1.ResourceCPU: p.CPU, corev1.ResourceMemory: p.Memory}
	requests := corev1.ResourceList{
		corev1.ResourceCPU:    fraction(p.CPU, 4, "10m"),
		corev1.ResourceMemory: fraction(p.Memory, 2, "16Mi"),
	}
	if p.ReserveMemory {
		requests[corev1.ResourceMemory] = p.Memory
	}
	// Device resources (devices.kubevirt.io/kvm) must have requests equal
	// to limits.
	for name, q := range p.Extra {
		limits[corev1.ResourceName(name)] = q
		requests[corev1.ResourceName(name)] = q
	}

	c := corev1.Container{
		Name:            ContainerName,
		Image:           p.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         p.Command,
		Args:            p.Args,
		Env:             envVars(p.Env),
		Resources:       corev1.ResourceRequirements{Limits: limits, Requests: requests},
		// A TTY keeps images whose default command is an interactive shell
		// (ubuntu, debian...) running even without a sleep override.
		Stdin: true,
		TTY:   true,
	}
	for _, port := range p.Ports {
		c.Ports = append(c.Ports, corev1.ContainerPort{Name: portName(port), ContainerPort: port, Protocol: corev1.ProtocolTCP})
	}
	if p.Privileged {
		yes := true
		c.SecurityContext = &corev1.SecurityContext{Privileged: &yes}
	} else {
		c.SecurityContext = &corev1.SecurityContext{
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		}
	}

	spec := corev1.PodSpec{
		Hostname:                      p.Name,
		AutomountServiceAccountToken:  &no,
		EnableServiceLinks:            &no,
		TerminationGracePeriodSeconds: &grace,
		Containers:                    []corev1.Container{c},
		NodeSelector:                  pol.NodeSelector,
		Tolerations:                   tolerations(pol.Tolerations),
		ImagePullSecrets:              pullSecrets(pol.ImagePullSecrets),
	}
	if p.Screen != nil && p.Screen.Sidecar == config.ScreenSidecarAndroid {
		spec.Containers = append(spec.Containers, androidScreen(p, pol))
	}
	if p.Disk != nil {
		spec.Volumes = []corev1.Volume{{
			Name: "data",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: dataClaimName(p.Name),
			}},
		}}
		spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
	}

	d := &appsv1.Deployment{
		ObjectMeta: p.meta(p.Name),
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: p.selector()},
			// The data volume is ReadWriteOnce: never run two pods at once.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: p.labels()},
				Spec:       spec,
			},
		},
	}
	d.Annotations = p.annotations()
	return d
}

// androidScreen is the sidecar that mirrors the Android display with scrcpy
// into a virtual X display and serves it over VNC. It reaches adbd over the
// pod's loopback, so it needs no privileges itself.
func androidScreen(p *plan, pol *config.Policy) corev1.Container {
	no := false
	return corev1.Container{
		Name:            ScreenContainerName,
		Image:           pol.AndroidScreenImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Env: []corev1.EnvVar{
			{Name: "ADB_TARGET", Value: "127.0.0.1:5555"},
			{Name: "VNC_PORT", Value: strconv.Itoa(int(p.Screen.Port))},
		},
		Ports: []corev1.ContainerPort{{Name: "vnc", ContainerPort: p.Screen.Port, Protocol: corev1.ProtocolTCP}},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &no,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}
}

// buildDataClaim renders the persistent /data volume of a container.
func buildDataClaim(p *plan, pol *config.Policy) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: p.meta(dataClaimName(p.Name)),
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: *p.Disk}},
		},
	}
	if pol.StorageClass != "" {
		sc := pol.StorageClass
		pvc.Spec.StorageClassName = &sc
	}
	return pvc
}

// buildVM renders a KubeVirt VirtualMachine. The root disk is a containerDisk
// (ephemeral: it resets when the VM stops) and cloud-init comes from a Secret.
func buildVM(p *plan, pol *config.Policy) *unstructured.Unstructured {
	cores := int64(math.Ceil(float64(p.CPU.MilliValue()) / 1000))
	if cores < 1 {
		cores = 1
	}
	podSpec := map[string]any{
		"domain": map[string]any{
			"cpu":    map[string]any{"cores": cores},
			"memory": map[string]any{"guest": p.Memory.String()},
			"devices": map[string]any{
				"disks": []any{
					map[string]any{"name": "rootdisk", "disk": map[string]any{"bus": "virtio"}},
					map[string]any{"name": "cloudinit", "disk": map[string]any{"bus": "virtio"}},
				},
				"interfaces": []any{map[string]any{"name": "default", "masquerade": map[string]any{}}},
			},
		},
		"networks":                      []any{map[string]any{"name": "default", "pod": map[string]any{}}},
		"terminationGracePeriodSeconds": int64(30),
		"volumes": []any{
			map[string]any{"name": "rootdisk", "containerDisk": map[string]any{"image": p.Image, "imagePullPolicy": "IfNotPresent"}},
			map[string]any{"name": "cloudinit", "cloudInitNoCloud": map[string]any{"secretRef": map[string]any{"name": p.Name}}},
		},
	}
	if len(pol.NodeSelector) > 0 {
		ns := map[string]any{}
		for k, v := range pol.NodeSelector {
			ns[k] = v
		}
		podSpec["nodeSelector"] = ns
	}
	if len(pol.Tolerations) > 0 {
		var ts []any
		for _, t := range pol.Tolerations {
			ts = append(ts, map[string]any{"key": t.Key, "operator": t.Operator, "value": t.Value, "effect": t.Effect})
		}
		podSpec["tolerations"] = ts
	}

	vm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata": map[string]any{
			"name":        p.Name,
			"labels":      stringMap(p.labels()),
			"annotations": stringMap(p.annotations()),
		},
		"spec": map[string]any{
			"runStrategy": "Always",
			"template": map[string]any{
				"metadata": map[string]any{"labels": stringMap(p.labels())},
				"spec":     podSpec,
			},
		},
	}}
	return vm
}

// buildVMSecret holds the cloud-init user data and the generated login.
func buildVMSecret(p *plan) (*corev1.Secret, error) {
	userData := p.CloudInit
	if userData == "" {
		doc := map[string]any{
			"hostname":     p.Name,
			"user":         p.User,
			"password":     p.Password,
			"chpasswd":     map[string]any{"expire": false},
			"ssh_pwauth":   true,
			"disable_root": true,
		}
		if p.SSHKey != "" {
			doc["ssh_authorized_keys"] = []string{p.SSHKey}
		}
		// Marshalling (not templating) keeps user input from injecting
		// extra cloud-init directives.
		b, err := yaml.Marshal(doc)
		if err != nil {
			return nil, err
		}
		userData = "#cloud-config\n" + string(b)
	}
	return &corev1.Secret{
		ObjectMeta: p.meta(p.Name),
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"userdata": userData,
			"user":     p.User,
			"password": p.Password,
		},
	}, nil
}

// buildService exposes the sandbox ports, or returns nil when there are none.
func buildService(p *plan) *corev1.Service {
	if len(p.Ports) == 0 {
		return nil
	}
	svc := &corev1.Service{
		ObjectMeta: p.meta(p.Name),
		Spec: corev1.ServiceSpec{
			Selector: p.selector(),
			Type:     corev1.ServiceTypeClusterIP,
		},
	}
	if p.Expose == ExposeNodePort {
		svc.Spec.Type = corev1.ServiceTypeNodePort
	}
	for _, port := range p.Ports {
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{
			Name:       portName(port),
			Port:       port,
			TargetPort: intstr.FromInt32(port),
			Protocol:   corev1.ProtocolTCP,
		})
	}
	return svc
}

// buildNetworkPolicy isolates one sandbox: inbound only on its exposed
// ports, outbound to DNS and (optionally) everything outside BlockCIDRs.
func buildNetworkPolicy(p *plan, pol *config.Policy) *netv1.NetworkPolicy {
	np := &netv1.NetworkPolicy{
		ObjectMeta: p.meta(p.Name),
		Spec: netv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: p.selector()},
			PolicyTypes: []netv1.PolicyType{netv1.PolicyTypeIngress, netv1.PolicyTypeEgress},
			Ingress:     []netv1.NetworkPolicyIngressRule{},
		},
	}
	if p.Screen != nil {
		// Only the Vishwakarma server reaches the screen port; users get it
		// through the console.
		pp := intstr.FromInt32(p.Screen.Port)
		tcp := corev1.ProtocolTCP
		from := metav1.LabelSelector{}
		if pol.ServerNamespace != "" {
			from.MatchLabels = map[string]string{"kubernetes.io/metadata.name": pol.ServerNamespace}
		}
		np.Spec.Ingress = append(np.Spec.Ingress, netv1.NetworkPolicyIngressRule{
			Ports: []netv1.NetworkPolicyPort{{Protocol: &tcp, Port: &pp}},
			From:  []netv1.NetworkPolicyPeer{{NamespaceSelector: &from}},
		})
	}
	if len(p.Ports) > 0 {
		rule := netv1.NetworkPolicyIngressRule{}
		for _, port := range p.Ports {
			pp := intstr.FromInt32(port)
			tcp := corev1.ProtocolTCP
			rule.Ports = append(rule.Ports, netv1.NetworkPolicyPort{Protocol: &tcp, Port: &pp})
		}
		np.Spec.Ingress = append(np.Spec.Ingress, rule)
	}

	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	dns := intstr.FromInt32(53)
	np.Spec.Egress = []netv1.NetworkPolicyEgressRule{{
		To: []netv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
		}},
		Ports: []netv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}},
	}}
	if pol.Network.AllowEgress {
		np.Spec.Egress = append(np.Spec.Egress, netv1.NetworkPolicyEgressRule{
			To: []netv1.NetworkPolicyPeer{{IPBlock: &netv1.IPBlock{CIDR: "0.0.0.0/0", Except: pol.Network.BlockCIDRs}}},
		})
	}
	return np
}

func ownerRef(apiVersion, kind, name string, uid types.UID) metav1.OwnerReference {
	yes := true
	return metav1.OwnerReference{APIVersion: apiVersion, Kind: kind, Name: name, UID: uid, BlockOwnerDeletion: &yes}
}

// fraction is q/div, but never below floor. Requests are a fraction of the
// limit so a small lab cluster can hold several idle sandboxes.
func fraction(q resource.Quantity, div int64, floor string) resource.Quantity {
	min := resource.MustParse(floor)
	v := resource.NewMilliQuantity(q.MilliValue()/div, q.Format)
	if v.Cmp(min) < 0 {
		return min
	}
	if q.Format == resource.BinarySI {
		return *resource.NewQuantity(v.Value(), resource.BinarySI)
	}
	return *v
}

func envVars(m map[string]string) []corev1.EnvVar {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]corev1.EnvVar, 0, len(keys))
	for _, k := range keys {
		out = append(out, corev1.EnvVar{Name: k, Value: m[k]})
	}
	return out
}

func tolerations(ts []config.Toleration) []corev1.Toleration {
	var out []corev1.Toleration
	for _, t := range ts {
		out = append(out, corev1.Toleration{
			Key: t.Key, Operator: corev1.TolerationOperator(t.Operator), Value: t.Value, Effect: corev1.TaintEffect(t.Effect),
		})
	}
	return out
}

func pullSecrets(names []string) []corev1.LocalObjectReference {
	var out []corev1.LocalObjectReference
	for _, n := range names {
		out = append(out, corev1.LocalObjectReference{Name: n})
	}
	return out
}

func portName(p int32) string { return "p" + strconv.Itoa(int(p)) }

func stringMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
