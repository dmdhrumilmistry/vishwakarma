// Package config loads the server configuration: process settings from the
// environment (secrets included) and the sandbox policy from a YAML file.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

// Kinds of sandbox.
const (
	KindContainer = "container"
	KindVM        = "vm"
	// KindMacOS is a macOS VM on a Mac host agent (Tart).
	KindMacOS = "macos"
)

// Auth modes.
const (
	AuthPassword = "password"
	AuthHeader   = "header"
)

// Config is the full server configuration.
type Config struct {
	Listen     string
	ConfigFile string
	Kubeconfig string
	LogLevel   string

	Auth   Auth
	Policy Policy

	// MacOSToken authenticates the server to macOS host agents.
	MacOSToken string
}

// Auth holds authentication settings. Secrets come only from the environment.
type Auth struct {
	Mode string
	// AdminPassword signs in the built-in "admin" user (password mode).
	AdminPassword string
	// APIToken is accepted as "Authorization: Bearer <token>" for automation.
	// It acts as the admin user.
	APIToken string
	// SessionKey signs session cookies. At least 32 bytes.
	SessionKey string
	// SessionTTL is how long a console session lasts.
	SessionTTL time.Duration
	// UserHeader carries the authenticated user name (header mode).
	UserHeader string
	// AdminUsers may see and manage every sandbox (header mode).
	AdminUsers []string
	// SecureCookies sets the Secure flag; disable only for plain-HTTP labs.
	SecureCookies bool
}

// Policy is the sandbox policy, read from the YAML config file.
type Policy struct {
	// Namespace sandboxes are created in.
	Namespace string `json:"namespace"`
	// PublicHost is shown as the address for NodePort endpoints, usually a
	// node IP or a DNS name that resolves to the nodes.
	PublicHost string `json:"publicHost"`

	DefaultTTL          Duration `json:"defaultTTL"`
	MaxTTL              Duration `json:"maxTTL"`
	MaxSandboxesPerUser int      `json:"maxSandboxesPerUser"`
	AllowCustomImages   bool     `json:"allowCustomImages"`
	AllowPrivileged     bool     `json:"allowPrivileged"`
	// AllowPrivilegedTemplates allows privileged containers only from
	// templates the operator marked privileged (Android needs it), with
	// their own image and command. AllowPrivileged allows any.
	AllowPrivilegedTemplates bool `json:"allowPrivilegedTemplates"`
	// MacOSOnLinux enables templates that run macOS on non-Apple hardware
	// (Docker-OSX). Apple's license only permits macOS on Apple hardware, so
	// this is off unless the operator turns it on. Such templates also need
	// /dev/kvm on the nodes.
	MacOSOnLinux bool `json:"macosOnLinux"`
	// AndroidScreenImage is the sidecar that streams Android screens to the
	// console. Defaults to the image built with this release.
	AndroidScreenImage string `json:"androidScreenImage"`
	// AndroidPlayStoreImage is a redroid image with Google Play
	// (images/android-playstore/build.sh); it adds the "Android 12 with Play
	// Store" template to the built-in catalogue. Empty removes it.
	AndroidPlayStoreImage string `json:"androidPlayStoreImage"`
	// AndroidPlayStoreUnrootedImage is the same without root (no su, a
	// "user" release-keys build), for apps that refuse rooted devices. It
	// adds "Android 12 with Play Store (unrooted)". Empty removes it.
	AndroidPlayStoreUnrootedImage string `json:"androidPlayStoreUnrootedImage"`
	// MacOSLinuxBaseImages maps a macOS flavor (ventura, tahoe...) to an
	// image carrying that installed system's disk (images/macos-base). With
	// MacOSOnLinux each adds a template that boots the system directly
	// instead of the installer. Keep the images private: they contain
	// Apple's operating system.
	MacOSLinuxBaseImages map[string]string `json:"macosLinuxBaseImages"`
	AllowNodePort        bool              `json:"allowNodePort"`
	StorageClass         string            `json:"storageClass"`

	Defaults Resources `json:"defaults"`
	Limits   Resources `json:"limits"`

	Network Network `json:"network"`
	VM      VM      `json:"vm"`
	MacOS   MacOS   `json:"macos"`

	// ImagePullSecrets are attached to every sandbox pod.
	ImagePullSecrets []string `json:"imagePullSecrets"`
	// NodeSelector and Tolerations place sandboxes on dedicated nodes.
	NodeSelector map[string]string `json:"nodeSelector"`
	Tolerations  []Toleration      `json:"tolerations"`

	Templates []Template `json:"templates"`

	// ServerNamespace is where the server runs (from VK_SERVER_NAMESPACE);
	// sandbox screen ports admit traffic from it only.
	ServerNamespace string `json:"-"`
}

// Resources is a CPU, memory and disk triple in Kubernetes quantity notation.
type Resources struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
	Disk   string `json:"disk"`
}

// Network controls the NetworkPolicy created for each sandbox.
type Network struct {
	// Isolate creates a NetworkPolicy per sandbox. Sandboxes then accept
	// traffic only on their exposed ports and cannot reach BlockCIDRs.
	Isolate bool `json:"isolate"`
	// BlockCIDRs are denied as egress destinations, typically the pod and
	// service CIDRs so sandboxes cannot reach cluster workloads.
	BlockCIDRs []string `json:"blockCIDRs"`
	// AllowEgress lets sandboxes reach the internet (everything outside
	// BlockCIDRs). DNS is always allowed.
	AllowEgress bool `json:"allowEgress"`
	// BlockAPIServer adds the API server endpoint addresses to BlockCIDRs at
	// startup. CNIs apply egress policy after Service DNAT, so blocking the
	// service CIDR alone does not stop a sandbox reaching the API server.
	BlockAPIServer bool `json:"blockAPIServer"`
}

// VM configures KubeVirt virtual machines.
type VM struct {
	// Enabled is auto (use KubeVirt when its API is present), true or false.
	Enabled string `json:"enabled"`
}

// MacOS configures macOS VMs, which run on Mac hosts through an agent.
type MacOS struct {
	// Agents are the Mac hosts. Empty disables macOS sandboxes.
	Agents []MacAgent `json:"agents"`
	// VNC gives every macOS VM a VNC endpoint for its screen.
	VNC bool `json:"vnc"`
}

// MacAgent is one Mac host running `vishwakarma agent`.
type MacAgent struct {
	Name string `json:"name"`
	// URL of the agent API, e.g. http://mac-mini.lan:8484.
	URL string `json:"url"`
	// InsecureSkipVerify accepts any TLS certificate from the agent.
	InsecureSkipVerify bool `json:"insecureSkipVerify"`
}

// Toleration mirrors the Kubernetes toleration fields that matter here.
type Toleration struct {
	Key      string `json:"key"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
	Effect   string `json:"effect"`
}

// Template is a preset users pick when creating a sandbox.
type Template struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
	Image       string `json:"image"`
	// Command and Args override the image entrypoint. Containers default to
	// a long sleep so base OS images stay up for interactive use.
	Command []string `json:"command"`
	Args    []string `json:"args"`
	// Shell started by the web terminal (containers). Empty picks bash, then sh.
	Shell string `json:"shell"`
	// User is the login user created by cloud-init (VMs) or baked into the
	// image (macOS).
	User string `json:"user"`
	// Password is the login baked into a macOS image. The agent replaces it
	// with a generated one on first boot unless KeepPassword is set.
	Password     string `json:"password"`
	KeepPassword bool   `json:"keepPassword"`
	// CloudInit replaces the generated cloud-init user data (VMs). Leave
	// empty to get a user with a generated password and your SSH key.
	CloudInit string `json:"cloudInit"`
	// Privileged runs the container privileged. Requires AllowPrivileged.
	Privileged bool `json:"privileged"`
	// Ports exposed by default.
	Ports []int32 `json:"ports"`
	// Resources overrides Policy.Defaults for this template.
	Resources Resources `json:"resources"`
	// Env is set in the container.
	Env map[string]string `json:"env"`
	// Screen gives the sandbox a Screen tab in the console (VNC).
	Screen *Screen `json:"screen,omitempty"`
	// ExtraResources are added to the container's requests and limits, e.g.
	// devices.kubevirt.io/kvm: "1" for /dev/kvm through KubeVirt's device
	// plugin.
	ExtraResources map[string]string `json:"extraResources,omitempty"`
	// MacOSOnLinux marks a template that runs macOS on non-Apple hardware;
	// it needs Policy.MacOSOnLinux.
	MacOSOnLinux bool `json:"macosOnLinux,omitempty"`
	// ReserveMemory requests the whole memory limit instead of half, so the
	// sandbox is only scheduled where that much is free. For workloads that
	// really use all of it, such as a QEMU guest: overcommitting those can
	// push a node into swap until it stops responding.
	ReserveMemory bool `json:"reserveMemory,omitempty"`
	// Init runs once before the sandbox starts, as an init container from
	// the same image with the same env and /data volume (for example to
	// create a disk image on the volume).
	Init []string `json:"init,omitempty"`
	// InitImage runs Init from another image (for example one that carries
	// a prebuilt disk) instead of the sandbox image.
	InitImage string `json:"initImage,omitempty"`
	// EmulatedEnv is added to Env when the sandbox runs without /dev/kvm.
	EmulatedEnv map[string]string `json:"emulatedEnv,omitempty"`
	// KVM is "prefer" (use /dev/kvm when a node has it, else run under
	// software emulation) or "require" (wait for a node with it). The device
	// comes from KubeVirt's device plugin (devices.kubevirt.io/kvm).
	KVM string `json:"kvm,omitempty"`
}

// KVM modes for templates.
const (
	KVMPrefer  = "prefer"
	KVMRequire = "require"
)

// Screen is how a container sandbox serves its display over VNC.
type Screen struct {
	// Port is the VNC port inside the pod.
	Port int32 `json:"port"`
	// Sidecar is "android" to add the Android screen sidecar (adb, scrcpy,
	// VNC), or empty when the main container serves VNC itself.
	Sidecar string `json:"sidecar,omitempty"`
}

// ScreenSidecarAndroid is the built-in Android screen sidecar.
const ScreenSidecarAndroid = "android"

// DefaultAndroidScreenImage is set by main to the image of this release.
var DefaultAndroidScreenImage = "docker.io/dmdhrumilmistry/vishwakarma-android-screen:latest"

// Duration is a time.Duration that reads "4h" style strings from YAML.
type Duration struct{ time.Duration }

// UnmarshalJSON accepts a Go duration string or a number of seconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		d.Duration = 0
		return nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		d.Duration = time.Duration(n) * time.Second
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = v
	return nil
}

// MarshalJSON writes the duration as a Go duration string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(d.Duration.String())), nil
}

// Load reads the environment and the policy file.
func Load() (*Config, error) {
	c := &Config{
		Listen:     env("VK_LISTEN", ":8080"),
		ConfigFile: env("VK_CONFIG", "/etc/vishwakarma/config.yaml"),
		Kubeconfig: env("VK_KUBECONFIG", os.Getenv("KUBECONFIG")),
		LogLevel:   env("VK_LOG_LEVEL", "info"),
		Auth: Auth{
			Mode:          env("VK_AUTH_MODE", AuthPassword),
			AdminPassword: os.Getenv("VK_ADMIN_PASSWORD"),
			APIToken:      os.Getenv("VK_API_TOKEN"),
			SessionKey:    os.Getenv("VK_SESSION_KEY"),
			UserHeader:    env("VK_AUTH_USER_HEADER", "X-Forwarded-User"),
			AdminUsers:    splitList(os.Getenv("VK_ADMIN_USERS")),
			SecureCookies: env("VK_SECURE_COOKIES", "true") == "true",
		},
		MacOSToken: os.Getenv("VK_MACOS_TOKEN"),
	}
	ttl, err := time.ParseDuration(env("VK_SESSION_TTL", "12h"))
	if err != nil {
		return nil, fmt.Errorf("VK_SESSION_TTL: %w", err)
	}
	c.Auth.SessionTTL = ttl

	raw, err := os.ReadFile(c.ConfigFile)
	switch {
	case errors.Is(err, os.ErrNotExist):
		raw = nil
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", c.ConfigFile, err)
	}
	p, err := ParsePolicy(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.ConfigFile, err)
	}
	c.Policy = *p
	if ns := os.Getenv("VK_NAMESPACE"); ns != "" {
		c.Policy.Namespace = ns
	}
	c.Policy.ServerNamespace = os.Getenv("VK_SERVER_NAMESPACE")
	return c, c.Validate()
}

// ParsePolicy parses policy YAML and fills in defaults. Empty input gives
// the default policy.
func ParsePolicy(raw []byte) (*Policy, error) {
	p := DefaultPolicy()
	if len(raw) > 0 {
		if err := yaml.UnmarshalStrict(raw, p); err != nil {
			return nil, err
		}
	}
	if p.Templates == nil {
		p.Templates = DefaultTemplates()
		if p.AndroidPlayStoreImage != "" {
			p.Templates = append(p.Templates, PlayStoreTemplate(p.AndroidPlayStoreImage, false))
		}
		if p.AndroidPlayStoreUnrootedImage != "" {
			p.Templates = append(p.Templates, PlayStoreTemplate(p.AndroidPlayStoreUnrootedImage, true))
		}
		flavors := make([]string, 0, len(p.MacOSLinuxBaseImages))
		for f := range p.MacOSLinuxBaseImages {
			flavors = append(flavors, f)
		}
		sort.Strings(flavors)
		for _, f := range flavors {
			if img := p.MacOSLinuxBaseImages[f]; img != "" {
				p.Templates = append(p.Templates, MacOSInstalledTemplate(f, img))
			}
		}
	}
	if p.AndroidScreenImage == "" {
		p.AndroidScreenImage = DefaultAndroidScreenImage
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// DefaultPolicy is the policy used when no config file is present.
func DefaultPolicy() *Policy {
	return &Policy{
		Namespace:           "vishwakarma-sandboxes",
		DefaultTTL:          Duration{4 * time.Hour},
		MaxTTL:              Duration{72 * time.Hour},
		MaxSandboxesPerUser: 5,
		AllowCustomImages:   true,
		AllowNodePort:       true,
		Defaults:            Resources{CPU: "1", Memory: "1Gi", Disk: "10Gi"},
		Limits:              Resources{CPU: "4", Memory: "8Gi", Disk: "50Gi"},
		Network: Network{
			Isolate:        true,
			AllowEgress:    true,
			BlockAPIServer: true,
			// k3s defaults; override for other distributions.
			BlockCIDRs: []string{"10.42.0.0/16", "10.43.0.0/16"},
		},
		VM:    VM{Enabled: "auto"},
		MacOS: MacOS{VNC: true},

		AndroidPlayStoreImage:         DefaultPlayStoreImage,
		AndroidPlayStoreUnrootedImage: DefaultPlayStoreUnrootedImage,
	}
}

// Published Play Store images (built by the release workflow).
const (
	DefaultPlayStoreImage         = "docker.io/dmdhrumilmistry/vishwakarma-redroid-playstore:12"
	DefaultPlayStoreUnrootedImage = "docker.io/dmdhrumilmistry/vishwakarma-redroid-playstore:12-unrooted"
)

// MacOSInstalledTemplate boots an installed macOS from a base disk image
// instead of the installer. The init container copies the disk onto the
// sandbox volume once; later starts reuse it. No recovery image, no boot
// picker: OpenCore boots the installed disk directly.
func MacOSInstalledTemplate(flavor, baseImage string) Template {
	var t Template
	for _, d := range DefaultTemplates() {
		if d.Name == "macos-linux" {
			t = d
		}
	}
	env := map[string]string{}
	for k, v := range t.Env {
		env[k] = v
	}
	env["NOPICKER"] = "true"
	// Docker-OSX downloads the recovery image unless this file exists;
	// the installed system does not need it.
	env["BASESYSTEM_IMAGE"] = "/data/.no-recovery"
	t.Env = env
	t.Name = "macos-" + flavor + "-linux"
	t.DisplayName = "macOS " + titleCase(flavor) + " on Linux (installed)"
	t.InitImage = baseImage
	t.Init = []string{"/bin/sh", "-c", "[ -e /data/mac_hdd_ng.img ] || cp /disk/mac_hdd_ng.img /data/mac_hdd_ng.img; touch /data/.no-recovery"}
	t.Description = "Installed macOS under QEMU (Docker-OSX), ready to use: the disk comes from a base image and is kept on the /data volume. Not licensed by Apple on non-Apple hardware"
	return t
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// PlayStoreTemplate is Android 12 with Google Play from the given image.
func PlayStoreTemplate(image string, unrooted bool) Template {
	var t Template
	for _, d := range DefaultTemplates() {
		if d.Name == "android-12" {
			t = d
		}
	}
	t.Name = "android-12-playstore"
	t.DisplayName = "Android 12 with Play Store"
	t.Image = image
	t.Resources = Resources{CPU: "2", Memory: "3Gi"}
	t.Description = "Android with Google Play. Register the device ID at google.com/android/uncertified before signing in"
	if unrooted {
		t.Name = "android-12-playstore-unrooted"
		t.DisplayName = "Android 12 with Play Store (unrooted)"
		t.Description = "Android with Google Play, no su and a release build, for apps that refuse rooted devices. Play Integrity device checks still fail"
	}
	return t
}

// DefaultTemplates is the built-in catalogue.
func DefaultTemplates() []Template {
	sleep := []string{"sleep", "infinity"}
	return []Template{
		{Name: "ubuntu-24.04", DisplayName: "Ubuntu 24.04", Kind: KindContainer, Image: "ubuntu:24.04", Command: sleep, Description: "Ubuntu userland in a container"},
		{Name: "debian-13", DisplayName: "Debian 13", Kind: KindContainer, Image: "debian:13", Command: sleep, Description: "Debian userland in a container"},
		{Name: "fedora", DisplayName: "Fedora", Kind: KindContainer, Image: "fedora:latest", Command: sleep, Description: "Fedora userland in a container"},
		{Name: "alpine", DisplayName: "Alpine", Kind: KindContainer, Image: "alpine:3", Command: sleep, Shell: "/bin/sh", Description: "Minimal Alpine container"},
		{Name: "kali", DisplayName: "Kali rolling", Kind: KindContainer, Image: "kalilinux/kali-rolling", Command: sleep, Description: "Kali Linux userland, tools not preinstalled"},
		{
			Name: "android-12", DisplayName: "Android 12", Kind: KindContainer,
			Image:       "redroid/redroid:12.0.0_64only-latest",
			Args:        []string{"androidboot.redroid_gpu_mode=guest"},
			Shell:       "/system/bin/sh",
			Privileged:  true,
			Ports:       []int32{5555},
			Screen:      &Screen{Port: 5900, Sidecar: ScreenSidecarAndroid},
			Resources:   Resources{CPU: "2", Memory: "2Gi"},
			Description: "Android in a container (redroid), screen in the browser. adb on port 5555. Needs the binder kernel module on the node",
		},
		{
			Name: "macos-linux", DisplayName: "macOS on Linux (Docker-OSX)", Kind: KindContainer,
			// Downloads the macOS recovery image from Apple on first start and
			// boots the installer; install macOS from the Screen tab.
			Image: "sickcodes/docker-osx:latest",
			Env: map[string]string{
				"SHORTNAME": "ventura",
				// Guest RAM in GB. QEMU's software emulation adds a
				// translation cache of up to 1 GiB on top, which the memory
				// limit below has to cover.
				"RAM":   "3",
				"EXTRA": "-display none -vnc 0.0.0.0:99",
				// The installed system lives on the persistent /data volume,
				// so it survives stop, start and restarts.
				"IMAGE_PATH": "/data/mac_hdd_ng.img",
			},
			Init:   []string{"/bin/sh", "-c", "[ -e /data/mac_hdd_ng.img ] || qemu-img create -f qcow2 /data/mac_hdd_ng.img 64G"},
			Ports:  []int32{10022},
			Screen: &Screen{Port: 5999},
			KVM:    KVMPrefer,
			// QEMU's software emulation of AVX and AES-NI trips macOS's
			// corecrypto self-test (kernel panic at boot); without KVM,
			// offer the guest plain SSE so it takes the generic code paths.
			EmulatedEnv: map[string]string{
				"CPUID_FLAGS": "vendor=GenuineIntel,+invtsc,vmware-cpuid-freq=on,+ssse3,+sse4.2,+popcnt,check,",
			},
			ReserveMemory: true,
			// 3 GB guest (RAM above), QEMU's translation cache and overhead.
			Resources:    Resources{CPU: "4", Memory: "4608Mi", Disk: "50Gi"},
			MacOSOnLinux: true,
			Description:  "macOS under QEMU (Docker-OSX): install it once from the Screen tab, it is kept on the /data volume. Fast with /dev/kvm on the node, very slow without. SSH on 10022 once Remote Login is on. Not licensed by Apple on non-Apple hardware",
		},
		{Name: "ubuntu-24.04-vm", DisplayName: "Ubuntu 24.04 VM", Kind: KindVM, Image: "quay.io/containerdisks/ubuntu:24.04", User: "ubuntu", Resources: Resources{Memory: "2Gi"}, Description: "Full Ubuntu VM with systemd and its own kernel"},
		{Name: "fedora-vm", DisplayName: "Fedora VM", Kind: KindVM, Image: "quay.io/containerdisks/fedora:latest", User: "fedora", Resources: Resources{Memory: "2Gi"}, Description: "Full Fedora VM"},
		{Name: "debian-12-vm", DisplayName: "Debian 12 VM", Kind: KindVM, Image: "quay.io/containerdisks/debian:12", User: "debian", Resources: Resources{Memory: "1Gi"}, Description: "Full Debian VM"},
		{Name: "macos-sequoia", DisplayName: "macOS Sequoia", Kind: KindMacOS, Image: "ghcr.io/cirruslabs/macos-sequoia-base:latest", User: "admin", Password: "admin", Resources: Resources{CPU: "4", Memory: "8Gi"}, Description: "macOS 15 VM on a Mac host. Terminal over SSH, screen over VNC"},
		{Name: "macos-tahoe", DisplayName: "macOS Tahoe", Kind: KindMacOS, Image: "ghcr.io/cirruslabs/macos-tahoe-base:latest", User: "admin", Password: "admin", Resources: Resources{CPU: "4", Memory: "8Gi"}, Description: "macOS 26 VM on a Mac host"},
		{Name: "macos-xcode", DisplayName: "macOS with Xcode (iOS Simulator)", Kind: KindMacOS, Image: "ghcr.io/cirruslabs/macos-sequoia-xcode:latest", User: "admin", Password: "admin", Resources: Resources{CPU: "4", Memory: "8Gi"}, Description: "Xcode and iOS simulators; run them with xcrun simctl or over VNC. First pull is large"},
		{Name: "cirros-vm", DisplayName: "CirrOS VM (tiny)", Kind: KindVM, Image: "quay.io/kubevirt/cirros-container-disk-demo:latest", User: "cirros", Resources: Resources{Memory: "256Mi"}, Description: "Tiny VM for smoke tests. Login cirros / gocubsgo"},
	}
}

var (
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	// Template names only live in annotations, so dots are fine.
	templateName = regexp.MustCompile(`^[a-z0-9]([-.a-z0-9]*[a-z0-9])?$`)
)

// ValidName reports whether s is a usable Kubernetes object name.
func ValidName(s string) bool { return len(s) <= 40 && dnsLabel.MatchString(s) }

// Validate checks the policy for mistakes an operator would want to hear about.
func (p *Policy) Validate() error {
	var errs []error
	if !dnsLabel.MatchString(p.Namespace) {
		errs = append(errs, fmt.Errorf("namespace %q is not a valid name", p.Namespace))
	}
	if p.DefaultTTL.Duration <= 0 || p.MaxTTL.Duration <= 0 {
		errs = append(errs, errors.New("defaultTTL and maxTTL must be positive"))
	} else if p.DefaultTTL.Duration > p.MaxTTL.Duration {
		errs = append(errs, errors.New("defaultTTL is longer than maxTTL"))
	}
	for name, r := range map[string]Resources{"defaults": p.Defaults, "limits": p.Limits} {
		for field, q := range map[string]string{"cpu": r.CPU, "memory": r.Memory, "disk": r.Disk} {
			if _, err := resource.ParseQuantity(q); err != nil {
				errs = append(errs, fmt.Errorf("%s.%s: %q is not a quantity", name, field, q))
			}
		}
	}
	agents := map[string]bool{}
	for i, a := range p.MacOS.Agents {
		if !ValidName(a.Name) || agents[a.Name] {
			errs = append(errs, fmt.Errorf("macos.agents[%d]: name must be a unique lowercase DNS label", i))
		}
		agents[a.Name] = true
		if !strings.HasPrefix(a.URL, "http://") && !strings.HasPrefix(a.URL, "https://") {
			errs = append(errs, fmt.Errorf("macos.agents[%d] (%s): url must start with http:// or https://", i, a.Name))
		}
	}
	switch p.VM.Enabled {
	case "auto", "true", "false":
	default:
		errs = append(errs, fmt.Errorf("vm.enabled must be auto, true or false, not %q", p.VM.Enabled))
	}
	seen := map[string]bool{}
	for i, t := range p.Templates {
		where := fmt.Sprintf("templates[%d] (%s)", i, t.Name)
		if len(t.Name) > 63 || !templateName.MatchString(t.Name) {
			errs = append(errs, fmt.Errorf("%s: name must be lowercase letters, digits, '-' and '.', at most 63 characters", where))
		}
		if seen[t.Name] {
			errs = append(errs, fmt.Errorf("%s: duplicate name", where))
		}
		seen[t.Name] = true
		if t.Kind != KindContainer && t.Kind != KindVM && t.Kind != KindMacOS {
			errs = append(errs, fmt.Errorf("%s: kind must be container, vm or macos", where))
		}
		if t.Image == "" {
			errs = append(errs, fmt.Errorf("%s: image is required", where))
		}
		if t.Screen != nil {
			if t.Kind != KindContainer {
				errs = append(errs, fmt.Errorf("%s: screen applies to containers; VMs and macOS have one already", where))
			}
			if t.Screen.Port < 1 || t.Screen.Port > 65535 {
				errs = append(errs, fmt.Errorf("%s: screen.port is out of range", where))
			}
			if t.Screen.Sidecar != "" && t.Screen.Sidecar != ScreenSidecarAndroid {
				errs = append(errs, fmt.Errorf("%s: screen.sidecar must be empty or %q", where, ScreenSidecarAndroid))
			}
		}
		switch t.KVM {
		case "", KVMPrefer, KVMRequire:
		default:
			errs = append(errs, fmt.Errorf("%s: kvm must be empty, prefer or require", where))
		}
		for k, q := range t.ExtraResources {
			if _, err := resource.ParseQuantity(q); err != nil {
				errs = append(errs, fmt.Errorf("%s: extraResources.%s: %q is not a quantity", where, k, q))
			}
		}
		for field, q := range map[string]string{"cpu": t.Resources.CPU, "memory": t.Resources.Memory, "disk": t.Resources.Disk} {
			if q == "" {
				continue
			}
			if _, err := resource.ParseQuantity(q); err != nil {
				errs = append(errs, fmt.Errorf("%s: resources.%s: %q is not a quantity", where, field, q))
			}
		}
	}
	return errors.Join(errs...)
}

// Template returns the named template.
func (p *Policy) Template(name string) (Template, bool) {
	for _, t := range p.Templates {
		if t.Name == name {
			return t, true
		}
	}
	return Template{}, false
}

// Validate checks process settings.
func (c *Config) Validate() error {
	var errs []error
	switch c.Auth.Mode {
	case AuthPassword:
		if len(c.Auth.AdminPassword) < 12 {
			errs = append(errs, errors.New("VK_ADMIN_PASSWORD must be at least 12 characters in password mode"))
		}
	case AuthHeader:
		if c.Auth.UserHeader == "" {
			errs = append(errs, errors.New("VK_AUTH_USER_HEADER is required in header mode"))
		}
	default:
		errs = append(errs, fmt.Errorf("VK_AUTH_MODE must be %s or %s", AuthPassword, AuthHeader))
	}
	if len(c.Auth.SessionKey) < 32 {
		errs = append(errs, errors.New("VK_SESSION_KEY must be at least 32 characters"))
	}
	if len(c.Policy.MacOS.Agents) > 0 && len(c.MacOSToken) < 24 {
		errs = append(errs, errors.New("VK_MACOS_TOKEN must be at least 24 characters when macos.agents are configured"))
	}
	if c.Auth.APIToken != "" && len(c.Auth.APIToken) < 24 {
		errs = append(errs, errors.New("VK_API_TOKEN must be at least 24 characters when set"))
	}
	return errors.Join(errs...)
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
