package macos

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/dmdhrumilmistry/vishwakarma/internal/config"
)

// Pool is the server's view of the Mac host agents.
type Pool struct {
	token string
	hosts []*host
}

type host struct {
	name   string
	base   string
	client *http.Client
	ws     *websocket.Dialer
}

// AgentError is an error reply from an agent.
type AgentError struct {
	Status int
	Msg    string
}

func (e *AgentError) Error() string { return e.Msg }

// NewPool returns a client for the configured agents, or nil when there are none.
func NewPool(agents []config.MacAgent, token string) *Pool {
	if len(agents) == 0 {
		return nil
	}
	p := &Pool{token: token}
	for _, a := range agents {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if a.InsecureSkipVerify {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // operator opt-in for self-signed agents
		}
		p.hosts = append(p.hosts, &host{
			name:   a.Name,
			base:   strings.TrimSuffix(a.URL, "/"),
			client: &http.Client{Transport: tr, Timeout: 30 * time.Second},
			ws:     &websocket.Dialer{TLSClientConfig: tr.TLSClientConfig, HandshakeTimeout: 15 * time.Second},
		})
	}
	return p
}

func (p *Pool) host(name string) (*host, error) {
	for _, h := range p.hosts {
		if h.name == name {
			return h, nil
		}
	}
	return nil, fmt.Errorf("unknown Mac host %q", name)
}

func (p *Pool) do(ctx context.Context, h *host, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("Mac host %s: %w", h.name, err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&e)
		if e.Error == "" {
			e.Error = res.Status
		}
		return &AgentError{Status: res.StatusCode, Msg: fmt.Sprintf("Mac host %s: %s", h.name, e.Error)}
	}
	if out != nil {
		return json.NewDecoder(res.Body).Decode(out)
	}
	return nil
}

// HostStatus is an agent's info, or why it could not be reached.
type HostStatus struct {
	Info
	Name  string `json:"name"`
	Error string `json:"error,omitempty"`
}

// Hosts asks every agent for its info, in parallel.
func (p *Pool) Hosts(ctx context.Context) []HostStatus {
	out := make([]HostStatus, len(p.hosts))
	var wg sync.WaitGroup
	for i, h := range p.hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			var info Info
			if err := p.do(ctx, h, "GET", "/v1/info", nil, &info); err != nil {
				out[i] = HostStatus{Name: h.name, Error: err.Error()}
				return
			}
			out[i] = HostStatus{Info: info, Name: h.name}
		}()
	}
	wg.Wait()
	return out
}

// List returns the VMs of every reachable agent. Unreachable agents are
// reported in the error but do not hide the others.
func (p *Pool) List(ctx context.Context) ([]VM, error) {
	type res struct {
		vms []VM
		err error
	}
	results := make([]res, len(p.hosts))
	var wg sync.WaitGroup
	for i, h := range p.hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			var vms []VM
			err := p.do(ctx, h, "GET", "/v1/vms", nil, &vms)
			for i := range vms {
				vms[i].Host = h.name
			}
			results[i] = res{vms, err}
		}()
	}
	wg.Wait()
	var all []VM
	var errs []error
	for _, r := range results {
		all = append(all, r.vms...)
		if r.err != nil {
			errs = append(errs, r.err)
		}
	}
	return all, errors.Join(errs...)
}

// Find returns the VM and the host it lives on.
func (p *Pool) Find(ctx context.Context, name string) (*VM, error) {
	vms, err := p.List(ctx)
	for i := range vms {
		if vms[i].Name == name {
			return &vms[i], nil
		}
	}
	if err != nil {
		return nil, err
	}
	return nil, ErrNotFound
}

// Create places the VM on the reachable host with the most free slots.
func (p *Pool) Create(ctx context.Context, req CreateRequest) (*VM, error) {
	hosts := p.Hosts(ctx)
	sort.SliceStable(hosts, func(i, j int) bool {
		return hosts[i].MaxRunning-hosts[i].Running > hosts[j].MaxRunning-hosts[j].Running
	})
	var errs []string
	for _, hs := range hosts {
		if hs.Error != "" {
			errs = append(errs, hs.Error)
			continue
		}
		h, _ := p.host(hs.Name)
		var vm VM
		err := p.do(ctx, h, "POST", "/v1/vms", req, &vm)
		var ae *AgentError
		if errors.As(err, &ae) && ae.Status == http.StatusConflict && strings.Contains(ae.Msg, ErrFull.Error()) {
			errs = append(errs, ae.Msg)
			continue // try the next Mac
		}
		if err != nil {
			return nil, err
		}
		vm.Host = h.name
		return &vm, nil
	}
	return nil, &AgentError{Status: http.StatusConflict, Msg: "no Mac host can take another VM: " + strings.Join(errs, "; ")}
}

func (p *Pool) call(ctx context.Context, hostName, method, path string, body, out any) error {
	h, err := p.host(hostName)
	if err != nil {
		return err
	}
	return p.do(ctx, h, method, path, body, out)
}

func (p *Pool) Delete(ctx context.Context, hostName, name string) error {
	return p.call(ctx, hostName, "DELETE", "/v1/vms/"+url.PathEscape(name), nil, nil)
}

func (p *Pool) SetRunning(ctx context.Context, hostName, name string, running bool) error {
	action := "stop"
	if running {
		action = "start"
	}
	return p.call(ctx, hostName, "POST", "/v1/vms/"+url.PathEscape(name)+"/"+action, nil, nil)
}

func (p *Pool) Extend(ctx context.Context, hostName, name string, at time.Time) error {
	return p.call(ctx, hostName, "POST", "/v1/vms/"+url.PathEscape(name)+"/extend", map[string]time.Time{"expiresAt": at}, nil)
}

func (p *Pool) Credentials(ctx context.Context, hostName, name string) (*Credentials, error) {
	var c Credentials
	if err := p.call(ctx, hostName, "GET", "/v1/vms/"+url.PathEscape(name)+"/credentials", nil, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Terminal opens the agent's terminal WebSocket for a VM.
func (p *Pool) Terminal(ctx context.Context, hostName, name string) (*websocket.Conn, error) {
	h, err := p.host(hostName)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(h.base + "/v1/vms/" + url.PathEscape(name) + "/terminal")
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	hdr := http.Header{"Authorization": {"Bearer " + p.token}}
	c, resp, err := h.ws.DialContext(ctx, u.String(), hdr)
	if err != nil {
		if resp != nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			return nil, fmt.Errorf("Mac host %s: %s: %s", h.name, resp.Status, strings.TrimSpace(string(b)))
		}
		return nil, fmt.Errorf("Mac host %s: %w", h.name, err)
	}
	return c, nil
}
