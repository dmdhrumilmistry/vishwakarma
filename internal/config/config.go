// Package config loads the server configuration: process settings from the
// environment (secrets included) and the sandbox policy from a YAML file.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
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
	AllowNodePort       bool     `json:"allowNodePort"`
	StorageClass        string   `json:"storageClass"`

	Defaults Resources `json:"defaults"`
	Limits   Resources `json:"limits"`

	Network Network `json:"network"`
	VM      VM      `json:"vm"`

	// ImagePullSecrets are attached to every sandbox pod.
	ImagePullSecrets []string `json:"imagePullSecrets"`
	// NodeSelector and Tolerations place sandboxes on dedicated nodes.
	NodeSelector map[string]string `json:"nodeSelector"`
	Tolerations  []Toleration      `json:"tolerations"`

	Templates []Template `json:"templates"`
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
	// User is the login user created by cloud-init (VMs).
	User string `json:"user"`
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
}

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
		VM: VM{Enabled: "auto"},
	}
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
		{Name: "ubuntu-24.04-vm", DisplayName: "Ubuntu 24.04 VM", Kind: KindVM, Image: "quay.io/containerdisks/ubuntu:24.04", User: "ubuntu", Resources: Resources{Memory: "2Gi"}, Description: "Full Ubuntu VM with systemd and its own kernel"},
		{Name: "fedora-vm", DisplayName: "Fedora VM", Kind: KindVM, Image: "quay.io/containerdisks/fedora:latest", User: "fedora", Resources: Resources{Memory: "2Gi"}, Description: "Full Fedora VM"},
		{Name: "debian-12-vm", DisplayName: "Debian 12 VM", Kind: KindVM, Image: "quay.io/containerdisks/debian:12", User: "debian", Resources: Resources{Memory: "1Gi"}, Description: "Full Debian VM"},
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
		if t.Kind != KindContainer && t.Kind != KindVM {
			errs = append(errs, fmt.Errorf("%s: kind must be container or vm", where))
		}
		if t.Image == "" {
			errs = append(errs, fmt.Errorf("%s: image is required", where))
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
