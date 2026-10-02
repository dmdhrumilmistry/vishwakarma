package config

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultPolicyIsValid(t *testing.T) {
	p, err := ParsePolicy(nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Namespace != "vishwakarma-sandboxes" || len(p.Templates) == 0 || p.DefaultTTL.Duration != 4*time.Hour {
		t.Fatalf("defaults %+v", p)
	}
}

func TestParsePolicy(t *testing.T) {
	p, err := ParsePolicy([]byte(`
namespace: labs
defaultTTL: 30m
maxTTL: 3600
allowPrivileged: true
templates:
  - name: osquery
    kind: container
    image: osquery/osquery:5
    privileged: true
    resources: {memory: 512Mi}
`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Namespace != "labs" || p.DefaultTTL.Duration != 30*time.Minute || p.MaxTTL.Duration != time.Hour {
		t.Errorf("parsed %+v", p)
	}
	if len(p.Templates) != 1 || !p.Templates[0].Privileged {
		t.Errorf("explicit templates must replace the defaults: %+v", p.Templates)
	}
	// Unset fields keep their defaults.
	if p.Limits.CPU != "4" || !p.Network.Isolate {
		t.Errorf("defaults lost: %+v", p)
	}
}

func TestParsePolicyErrors(t *testing.T) {
	cases := map[string]string{
		"unknown field":     "nmespace: x",
		"bad namespace":     "namespace: Bad_NS",
		"ttl order":         "defaultTTL: 10h\nmaxTTL: 1h",
		"bad duration":      "defaultTTL: soon",
		"bad quantity":      "limits: {cpu: lots, memory: 1Gi, disk: 1Gi}",
		"vm enabled":        "vm: {enabled: maybe}",
		"template kind":     "templates: [{name: a, kind: pod, image: x}]",
		"template image":    "templates: [{name: a, kind: container}]",
		"template dup":      "templates: [{name: a, kind: container, image: x}, {name: a, kind: vm, image: y}]",
		"template name":     "templates: [{name: Bad, kind: container, image: x}]",
		"template quantity": "templates: [{name: a, kind: container, image: x, resources: {cpu: many}}]",
	}
	for name, raw := range cases {
		if _, err := ParsePolicy([]byte(raw)); err == nil {
			t.Errorf("%s: accepted %q", name, raw)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	good := Config{Auth: Auth{Mode: AuthPassword, AdminPassword: strings.Repeat("p", 12), SessionKey: strings.Repeat("k", 32)}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []Config{
		{Auth: Auth{Mode: AuthPassword, AdminPassword: "short", SessionKey: strings.Repeat("k", 32)}},
		{Auth: Auth{Mode: AuthPassword, AdminPassword: strings.Repeat("p", 12), SessionKey: "short"}},
		{Auth: Auth{Mode: "none", SessionKey: strings.Repeat("k", 32)}},
		{Auth: Auth{Mode: AuthHeader, SessionKey: strings.Repeat("k", 32)}},
		{Auth: Auth{Mode: AuthHeader, UserHeader: "X-User", SessionKey: strings.Repeat("k", 32), APIToken: "short"}},
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("case %d accepted: %+v", i, c.Auth)
		}
	}
}
