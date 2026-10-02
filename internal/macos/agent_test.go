package macos

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/dmdhrumilmistry/vishwakarma/internal/config"
)

const token = "agent-token-agent-token-agent-token"

type fixture struct {
	agent *Agent
	pool  *Pool
	srv   *httptest.Server
	state string
}

// newFixture runs an agent with the simulator behind httptest and a Pool
// pointed at it, which is exactly how the server talks to a Mac.
func newFixture(t *testing.T, max int) *fixture {
	t.Helper()
	sim := NewSimulator()
	sim.PrepareDelay = 10 * time.Millisecond
	state := filepath.Join(t.TempDir(), "agent.json")
	a, err := NewAgent(AgentConfig{
		Name: "mac1", PublicHost: "127.0.0.1", Token: token, StatePath: state,
		BindAddr: "127.0.0.1", PortMin: 41000, PortMax: 41999, MaxRunning: max,
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
		// Deletes finish in the background and rewrite the state file;
		// wait so the temp dir can be removed.
		for deadline := time.Now().Add(5 * time.Second); len(a.List()) > 0 && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
		a.saveMu.Lock()
		defer a.saveMu.Unlock()
	})
	pool := NewPool([]config.MacAgent{{Name: "mac1", URL: srv.URL}}, token)
	return &fixture{agent: a, pool: pool, srv: srv, state: state}
}

func req(name string) CreateRequest {
	return CreateRequest{
		Name: name, Owner: "alice", Image: "ghcr.io/cirruslabs/macos-sequoia-base:latest",
		CPU: 4, MemoryMB: 8192, User: "admin", Password: "admin", Ports: []int32{8080}, VNC: true,
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
	}
}

func waitState(t *testing.T, f *fixture, name, want string) VM {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		vm, err := f.pool.Find(context.Background(), name)
		if err == nil && vm.State == want {
			return *vm
		}
		time.Sleep(20 * time.Millisecond)
	}
	vm, err := f.pool.Find(context.Background(), name)
	t.Fatalf("%s never became %s: %+v %v", name, want, vm, err)
	return VM{}
}

func readBanner(t *testing.T, addr string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	line, _ := bufio.NewReader(c).ReadString('\n')
	return line
}

func TestLifecycleAndForwarding(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()
	vm, err := f.pool.Create(ctx, req("mac-a"))
	if err != nil {
		t.Fatal(err)
	}
	if vm.Host != "mac1" || vm.State != StatePending {
		t.Fatalf("created %+v", vm)
	}

	vm2 := waitState(t, f, "mac-a", StateRunning)
	names := map[string]Forward{}
	for _, fw := range vm2.Forwards {
		names[fw.Name] = fw
	}
	if len(names) != 3 || names[ForwardSSH].GuestPort != 22 || names[ForwardVNC].GuestPort != 5900 || names["p8080"].GuestPort != 8080 {
		t.Fatalf("forwards %+v: want ssh, vnc and p8080", vm2.Forwards)
	}
	// Connections to the host ports reach the guest ports.
	if b := readBanner(t, fmt.Sprintf("127.0.0.1:%d", names[ForwardSSH].HostPort)); !strings.HasPrefix(b, "SSH-2.0-") {
		t.Errorf("ssh forward banner %q", b)
	}
	if b := readBanner(t, fmt.Sprintf("127.0.0.1:%d", names[ForwardVNC].HostPort)); !strings.HasPrefix(b, "RFB ") {
		t.Errorf("vnc forward banner %q", b)
	}

	creds, err := f.pool.Credentials(ctx, "mac1", "mac-a")
	if err != nil {
		t.Fatal(err)
	}
	if creds.Password == "admin" || len(creds.Password) != 16 || creds.VNCPassword == "" {
		t.Errorf("first boot must replace the image password and set a VNC password: %+v", creds)
	}

	if err := f.pool.SetRunning(ctx, "mac1", "mac-a", false); err != nil {
		t.Fatal(err)
	}
	waitState(t, f, "mac-a", StateStopped)
	if _, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", names[ForwardSSH].HostPort), time.Second); err == nil {
		t.Error("forwards must close when the VM stops")
	}
	if err := f.pool.SetRunning(ctx, "mac1", "mac-a", true); err != nil {
		t.Fatal(err)
	}
	vm3 := waitState(t, f, "mac-a", StateRunning)
	for _, fw := range vm3.Forwards {
		if fw.HostPort != names[fw.Name].HostPort {
			t.Errorf("%s moved from host port %d to %d across a restart", fw.Name, names[fw.Name].HostPort, fw.HostPort)
		}
	}
	creds2, _ := f.pool.Credentials(ctx, "mac1", "mac-a")
	if creds2.Password != creds.Password {
		t.Error("the password must be rotated once, not on every boot")
	}

	at := time.Now().Add(5 * time.Hour).UTC().Truncate(time.Second)
	if err := f.pool.Extend(ctx, "mac1", "mac-a", at); err != nil {
		t.Fatal(err)
	}
	if vm, _ := f.pool.Find(ctx, "mac-a"); !vm.ExpiresAt.Equal(at) {
		t.Errorf("expiry %v, want %v", vm.ExpiresAt, at)
	}

	if err := f.pool.Delete(ctx, "mac1", "mac-a"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := f.pool.Find(ctx, "mac-a"); errors.Is(err, ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("vm still listed after delete")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCapacity(t *testing.T) {
	f := newFixture(t, 1)
	ctx := context.Background()
	if _, err := f.pool.Create(ctx, req("one")); err != nil {
		t.Fatal(err)
	}
	_, err := f.pool.Create(ctx, req("two"))
	var ae *AgentError
	if !errors.As(err, &ae) || ae.Status != http.StatusConflict || !strings.Contains(err.Error(), "no Mac host") {
		t.Fatalf("second VM on a full host: %v", err)
	}
	if _, err := f.pool.Create(ctx, req("one")); err == nil {
		t.Error("duplicate name accepted")
	}
}

func TestAgentAuth(t *testing.T) {
	f := newFixture(t, 2)
	for _, h := range []string{"", "Bearer wrong", token} {
		r, _ := http.NewRequest("GET", f.srv.URL+"/v1/vms", nil)
		if h != "" {
			r.Header.Set("Authorization", h)
		}
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("Authorization %q: %d, want 401", h, res.StatusCode)
		}
	}
	bad := NewPool([]config.MacAgent{{Name: "mac1", URL: f.srv.URL}}, "not-the-token-not-the-token")
	if _, err := bad.List(context.Background()); err == nil {
		t.Error("pool with the wrong token listed VMs")
	}
}

func TestValidation(t *testing.T) {
	f := newFixture(t, 2)
	f.agent.cfg.AllowImages = []string{"ghcr.io/cirruslabs/"}
	cases := map[string]func(*CreateRequest){
		"bad name":      func(r *CreateRequest) { r.Name = "Bad_Name" },
		"no image":      func(r *CreateRequest) { r.Image = "" },
		"image blocked": func(r *CreateRequest) { r.Image = "evil.example/macos:1" },
		"no user":       func(r *CreateRequest) { r.User = "" },
		"bad port":      func(r *CreateRequest) { r.Ports = []int32{0} },
	}
	for name, mut := range cases {
		r := req("v")
		mut(&r)
		if _, err := f.agent.Create(r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestReapAndStatePersistence(t *testing.T) {
	f := newFixture(t, 2)
	r := req("old")
	r.ExpiresAt = time.Now().Add(-time.Hour)
	if _, err := f.agent.Create(r); err != nil {
		t.Fatal(err)
	}
	if _, err := f.agent.Create(req("keep")); err != nil {
		t.Fatal(err)
	}
	waitState(t, f, "keep", StateRunning)

	// A new agent on the same state file sees both VMs.
	b, err := NewAgent(f.agent.cfg, NewSimulator(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(b.List()); n != 2 {
		t.Fatalf("reloaded %d VMs, want 2", n)
	}

	f.agent.reap()
	deadline := time.Now().Add(5 * time.Second)
	for len(f.agent.List()) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("after reap: %v", f.agent.List())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if f.agent.List()[0].Name != "keep" {
		t.Errorf("reaped the wrong VM: %v", f.agent.List())
	}
}

func TestTerminalThroughPool(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()
	if _, err := f.pool.Create(ctx, req("term")); err != nil {
		t.Fatal(err)
	}
	waitState(t, f, "term", StateRunning)
	ws, err := f.pool.Terminal(ctx, "mac1", "term")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":100,"rows":30}`)); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("sw_vers\r")); err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	for !strings.Contains(got.String(), "ProductName") {
		_, data, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("terminal output so far %q: %v", got.String(), err)
		}
		got.Write(data)
	}
}

func TestParseVNC(t *testing.T) {
	cases := map[string][2]string{
		"Opening vnc://:s3cret@127.0.0.1:59322...": {"127.0.0.1:59322", "s3cret"},
		"vnc://127.0.0.1:5901":                     {"127.0.0.1:5901", ""},
	}
	for line, want := range cases {
		addr, pw, ok := parseVNC(line)
		if !ok || addr != want[0] || pw != want[1] {
			t.Errorf("parseVNC(%q) = %q %q %v", line, addr, pw, ok)
		}
	}
	if _, _, ok := parseVNC("booting..."); ok {
		t.Error("matched a line without a URL")
	}
}

func TestParseList(t *testing.T) {
	vms, err := parseList([]byte(`[{"Name":"vk-a","State":"running","Disk":50},{"Name":"vk-b","Running":true},{"Name":"vk-c","State":"stopped"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if !vms[0].running() || !vms[1].running() || vms[2].running() {
		t.Errorf("running flags wrong: %+v", vms)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote(`it's`); got != `'it'\''s'` {
		t.Errorf("shellQuote = %s", got)
	}
}
