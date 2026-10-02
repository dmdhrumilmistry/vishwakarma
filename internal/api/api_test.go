package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dmdhrumilmistry/vishwakarma/internal/auth"
	"github.com/dmdhrumilmistry/vishwakarma/internal/config"
	"github.com/dmdhrumilmistry/vishwakarma/internal/sandbox"
)

const password = "correct-horse-battery"

func newServer(t *testing.T, mode string) *httptest.Server {
	t.Helper()
	pol := config.DefaultPolicy()
	pol.Templates = config.DefaultTemplates()
	pol.MaxSandboxesPerUser = 1
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		sandbox.VMGVR:  "VirtualMachineList",
		sandbox.VMIGVR: "VirtualMachineInstanceList",
	})
	mgr := sandbox.NewManager(fake.NewClientset(), dyn, pol, func(context.Context) bool { return false }, nil)
	a := auth.New(config.Auth{
		Mode: mode, AdminPassword: password, SessionKey: strings.Repeat("k", 32),
		SessionTTL: time.Hour, UserHeader: "X-Forwarded-User", APIToken: "token-token-token-token-token",
	})
	mux := http.NewServeMux()
	New(mgr, a, nil, "test", nil).Routes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

type client struct {
	t      *testing.T
	base   string
	cookie *http.Cookie
	header map[string]string
}

func (c *client) do(method, path, body string, csrf bool) (int, map[string]any) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf {
		req.Header.Set(CSRFHeader, "1")
	}
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	for k, v := range c.header {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	for _, ck := range res.Cookies() {
		if ck.Name == auth.CookieName {
			c.cookie = ck
		}
	}
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestRequiresAuth(t *testing.T) {
	ts := newServer(t, config.AuthPassword)
	c := &client{t: t, base: ts.URL}
	for _, p := range []string{"/api/v1/info", "/api/v1/sandboxes", "/api/v1/sandboxes/x", "/api/v1/sandboxes/x/terminal"} {
		if code, _ := c.do("GET", p, "", false); code != http.StatusUnauthorized {
			t.Errorf("GET %s: %d, want 401", p, code)
		}
	}
}

func TestLoginCreateList(t *testing.T) {
	ts := newServer(t, config.AuthPassword)
	c := &client{t: t, base: ts.URL}

	if code, _ := c.do("POST", "/api/v1/login", `{"password":"`+password+`"}`, false); code != http.StatusForbidden {
		t.Errorf("login without CSRF header: %d, want 403", code)
	}
	if code, _ := c.do("POST", "/api/v1/login", `{"password":"wrong"}`, true); code != http.StatusUnauthorized {
		t.Errorf("wrong password: %d", code)
	}
	if code, _ := c.do("POST", "/api/v1/login", `{"password":"`+password+`"}`, true); code != http.StatusOK || c.cookie == nil {
		t.Fatalf("login: %d", code)
	}

	code, info := c.do("GET", "/api/v1/info", "", false)
	if code != 200 || info["user"] != auth.AdminUser || info["vms"] != false {
		t.Fatalf("info %d %v", code, info)
	}
	for _, tpl := range info["templates"].([]any) {
		if tpl.(map[string]any)["kind"] == config.KindVM {
			t.Error("VM templates must be hidden when KubeVirt is unavailable")
		}
	}

	spec := `{"name":"demo","template":"alpine","ports":[80]}`
	if code, _ := c.do("POST", "/api/v1/sandboxes", spec, false); code != http.StatusForbidden {
		t.Errorf("create without CSRF header: %d, want 403", code)
	}
	code, sb := c.do("POST", "/api/v1/sandboxes", spec, true)
	if code != http.StatusCreated || sb["name"] != "demo" {
		t.Fatalf("create %d %v", code, sb)
	}
	if code, body := c.do("POST", "/api/v1/sandboxes", spec, true); code != http.StatusConflict {
		t.Errorf("duplicate: %d %v", code, body)
	}
	if code, body := c.do("POST", "/api/v1/sandboxes", `{"name":"x","template":"alpine","cpu":"64"}`, true); code != http.StatusBadRequest || !strings.Contains(body["error"].(string), "limit") {
		t.Errorf("over limit: %d %v", code, body)
	}
	if code, _ := c.do("POST", "/api/v1/sandboxes", `{"name":"x","bogus":1}`, true); code != http.StatusBadRequest {
		t.Errorf("unknown field: %d", code)
	}

	code, list := c.do("GET", "/api/v1/sandboxes", "", false)
	if code != 200 || len(list["items"].([]any)) != 1 {
		t.Fatalf("list %d %v", code, list)
	}
	if code, _ := c.do("DELETE", "/api/v1/sandboxes/demo", "", true); code != http.StatusNoContent {
		t.Errorf("delete: %d", code)
	}
	if code, _ := c.do("GET", "/api/v1/sandboxes/demo", "", false); code != http.StatusNotFound {
		t.Errorf("get after delete: %d", code)
	}
}

func TestBearerSkipsCSRF(t *testing.T) {
	ts := newServer(t, config.AuthPassword)
	c := &client{t: t, base: ts.URL, header: map[string]string{"Authorization": "Bearer token-token-token-token-token"}}
	if code, body := c.do("POST", "/api/v1/sandboxes", `{"name":"ci","template":"alpine"}`, false); code != http.StatusCreated {
		t.Fatalf("bearer create: %d %v", code, body)
	}
}

func TestHeaderModeQuotaAndIsolation(t *testing.T) {
	ts := newServer(t, config.AuthHeader)
	alice := &client{t: t, base: ts.URL, header: map[string]string{"X-Forwarded-User": "alice"}}
	bob := &client{t: t, base: ts.URL, header: map[string]string{"X-Forwarded-User": "bob"}}

	if code, _ := alice.do("POST", "/api/v1/sandboxes", `{"name":"a1","template":"alpine"}`, true); code != http.StatusCreated {
		t.Fatalf("alice create: %d", code)
	}
	if code, body := alice.do("POST", "/api/v1/sandboxes", `{"name":"a2","template":"alpine"}`, true); code != http.StatusForbidden {
		t.Errorf("over quota: %d %v", code, body)
	}
	if code, _ := bob.do("GET", "/api/v1/sandboxes/a1", "", false); code != http.StatusNotFound {
		t.Errorf("bob reads alice's sandbox: %d, want 404", code)
	}
	if code, _ := bob.do("DELETE", "/api/v1/sandboxes/a1", "", true); code != http.StatusNotFound {
		t.Errorf("bob deletes alice's sandbox: %d, want 404", code)
	}
	if code, _ := bob.do("POST", "/api/v1/login", `{"password":"x"}`, true); code != http.StatusBadRequest {
		t.Errorf("password login in header mode: %d", code)
	}
}

func TestSameOrigin(t *testing.T) {
	r := httptest.NewRequest("GET", "http://vk.example.com/api/v1/sandboxes/x/terminal", nil)
	r.Host = "vk.example.com"
	r.Header.Set("Origin", "https://vk.example.com")
	if !sameOrigin(r) {
		t.Error("same origin rejected")
	}
	r.Header.Set("Origin", "https://evil.example.net")
	if sameOrigin(r) {
		t.Error("cross origin accepted")
	}
	r.Header.Del("Origin")
	if sameOrigin(r) {
		t.Error("no origin without a bearer token accepted")
	}
	r.Header.Set("Authorization", "Bearer t")
	if !sameOrigin(r) {
		t.Error("no origin with a bearer token rejected")
	}
}
