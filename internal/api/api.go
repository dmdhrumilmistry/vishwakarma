// Package api is the REST API and the web terminal.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/dmdhrumilmistry/vishwakarma/internal/auth"
	"github.com/dmdhrumilmistry/vishwakarma/internal/config"
	"github.com/dmdhrumilmistry/vishwakarma/internal/kube"
	"github.com/dmdhrumilmistry/vishwakarma/internal/sandbox"
)

// CSRFHeader must accompany state-changing requests that are not bearer
// authenticated. Browsers cannot send a custom header cross-origin without
// a CORS preflight, which this server never approves.
const CSRFHeader = "X-Vishwakarma-Request"

// Server serves the API.
type Server struct {
	mgr     *sandbox.Manager
	auth    *auth.Authenticator
	clients *kube.Clients
	log     *slog.Logger
	version string
}

// New returns an API server. clients may be nil in tests that do not open
// terminals or read logs.
func New(mgr *sandbox.Manager, a *auth.Authenticator, clients *kube.Clients, version string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{mgr: mgr, auth: a, clients: clients, log: log, version: version}
}

// Routes registers the API on mux.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/login", s.login)
	mux.HandleFunc("POST /api/v1/logout", s.logout)
	mux.HandleFunc("GET /api/v1/info", s.authed(s.info))
	mux.HandleFunc("GET /api/v1/sandboxes", s.authed(s.list))
	mux.HandleFunc("POST /api/v1/sandboxes", s.authed(s.create))
	mux.HandleFunc("GET /api/v1/sandboxes/{name}", s.authed(s.get))
	mux.HandleFunc("DELETE /api/v1/sandboxes/{name}", s.authed(s.delete))
	mux.HandleFunc("POST /api/v1/sandboxes/{name}/start", s.authed(s.power(true)))
	mux.HandleFunc("POST /api/v1/sandboxes/{name}/stop", s.authed(s.power(false)))
	mux.HandleFunc("POST /api/v1/sandboxes/{name}/extend", s.authed(s.extend))
	mux.HandleFunc("GET /api/v1/sandboxes/{name}/credentials", s.authed(s.credentials))
	mux.HandleFunc("GET /api/v1/sandboxes/{name}/logs", s.authed(s.logs))
	mux.HandleFunc("GET /api/v1/sandboxes/{name}/terminal", s.authed(s.terminal))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint")
	})
}

type handler func(w http.ResponseWriter, r *http.Request, c sandbox.Caller)

func (s *Server) authed(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.auth.Caller(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "sign in first")
			return
		}
		if !bearer(r) && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(CSRFHeader) == "" {
			writeError(w, http.StatusForbidden, "missing "+CSRFHeader+" header")
			return
		}
		h(w, r, c)
	}
}

func bearer(r *http.Request) bool { return strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") }

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(CSRFHeader) == "" {
		writeError(w, http.StatusForbidden, "missing "+CSRFHeader+" header")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	switch err := s.auth.Login(w, r, body.Password); {
	case errors.Is(err, auth.ErrLocked):
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, auth.ErrBadLogin):
		writeError(w, http.StatusUnauthorized, err.Error())
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]string{"user": auth.AdminUser})
	}
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.auth.Logout(w)
	w.WriteHeader(http.StatusNoContent)
}

// templateView is a template without operator-only fields.
type templateView struct {
	Name        string           `json:"name"`
	DisplayName string           `json:"displayName"`
	Description string           `json:"description,omitempty"`
	Kind        string           `json:"kind"`
	Image       string           `json:"image"`
	User        string           `json:"user,omitempty"`
	Privileged  bool             `json:"privileged,omitempty"`
	Ports       []int32          `json:"ports,omitempty"`
	Resources   config.Resources `json:"resources"`
}

func (s *Server) info(w http.ResponseWriter, r *http.Request, c sandbox.Caller) {
	p := s.mgr.Policy()
	vms := s.mgr.VMsAvailable(r.Context())
	tpls := []templateView{}
	for _, t := range p.Templates {
		if t.Kind == config.KindVM && !vms {
			continue
		}
		name := t.DisplayName
		if name == "" {
			name = t.Name
		}
		tpls = append(tpls, templateView{
			Name: t.Name, DisplayName: name, Description: t.Description, Kind: t.Kind, Image: t.Image,
			User: t.User, Privileged: t.Privileged, Ports: t.Ports, Resources: t.Resources,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":   s.version,
		"user":      c.Name,
		"admin":     c.Admin,
		"authMode":  s.auth.Mode(),
		"namespace": p.Namespace,
		"vms":       vms,
		"templates": tpls,
		"policy": map[string]any{
			"defaultTTL":          p.DefaultTTL,
			"maxTTL":              p.MaxTTL,
			"maxSandboxesPerUser": p.MaxSandboxesPerUser,
			"allowCustomImages":   p.AllowCustomImages,
			"allowPrivileged":     p.AllowPrivileged,
			"allowNodePort":       p.AllowNodePort,
			"publicHost":          p.PublicHost,
			"defaults":            p.Defaults,
			"limits":              p.Limits,
			"isolated":            p.Network.Isolate,
		},
	})
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, c sandbox.Caller) {
	list, err := s.mgr.List(r.Context(), c)
	if err != nil {
		s.fail(w, err)
		return
	}
	if list == nil {
		list = []sandbox.Sandbox{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (s *Server) create(w http.ResponseWriter, r *http.Request, c sandbox.Caller) {
	var spec sandbox.Spec
	if !readJSON(w, r, &spec) {
		return
	}
	sb, err := s.mgr.Create(r.Context(), spec, c)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sb)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, c sandbox.Caller) {
	sb, err := s.mgr.Get(r.Context(), r.PathValue("name"), c)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request, c sandbox.Caller) {
	if err := s.mgr.Delete(r.Context(), r.PathValue("name"), c); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) power(running bool) handler {
	return func(w http.ResponseWriter, r *http.Request, c sandbox.Caller) {
		sb, err := s.mgr.SetRunning(r.Context(), r.PathValue("name"), running, c)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, sb)
	}
}

func (s *Server) extend(w http.ResponseWriter, r *http.Request, c sandbox.Caller) {
	var body struct {
		TTL string `json:"ttl"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	sb, err := s.mgr.Extend(r.Context(), r.PathValue("name"), body.TTL, c)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (s *Server) credentials(w http.ResponseWriter, r *http.Request, c sandbox.Caller) {
	cr, err := s.mgr.Credentials(r.Context(), r.PathValue("name"), c)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, cr)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request, c sandbox.Caller) {
	sb, err := s.mgr.Get(r.Context(), r.PathValue("name"), c)
	if err != nil {
		s.fail(w, err)
		return
	}
	if sb.Kind != config.KindContainer {
		writeError(w, http.StatusBadRequest, "logs are available for containers; open the console of a VM instead")
		return
	}
	pod, err := s.mgr.RunningPod(r.Context(), sb)
	if err != nil {
		s.fail(w, err)
		return
	}
	tail := int64(500)
	if v, err := strconv.ParseInt(r.URL.Query().Get("tail"), 10, 64); err == nil && v > 0 && v <= 5000 {
		tail = v
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	stream, err := s.clients.Kube.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: sandbox.ContainerName, TailLines: &tail,
	}).Stream(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer stream.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, io.LimitReader(stream, 4<<20))
}

// fail maps an error to a response. Kubernetes errors are passed through
// because they usually say exactly what is wrong (quota, admission, RBAC).
func (s *Server) fail(w http.ResponseWriter, err error) {
	var inv *sandbox.InvalidError
	var quota *sandbox.QuotaError
	switch {
	case errors.As(err, &inv):
		writeError(w, http.StatusBadRequest, inv.Msg)
	case errors.As(err, &quota):
		writeError(w, http.StatusForbidden, quota.Error())
	case errors.Is(err, sandbox.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, sandbox.ErrExists):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, sandbox.ErrUnavailable):
		writeError(w, http.StatusBadRequest, err.Error())
	case apierrors.IsForbidden(err):
		writeError(w, http.StatusForbidden, err.Error())
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		writeError(w, http.StatusBadRequest, err.Error())
	case apierrors.IsNotFound(err):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		s.log.Error("request failed", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
