// Package auth identifies the caller of each request.
//
// Two modes:
//   - password: one built-in "admin" user signs in with a password and gets a
//     signed session cookie. Automation uses the API token as a bearer token.
//   - header: a trusted reverse proxy (oauth2-proxy, Authelia, Pomerium...)
//     authenticates users and passes the user name in a header. Never expose
//     the server directly in this mode: anyone could set the header.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dmdhrumilmistry/vishwakarma/internal/config"
	"github.com/dmdhrumilmistry/vishwakarma/internal/sandbox"
)

// CookieName is the session cookie.
const CookieName = "vk_session"

// AdminUser is the built-in user in password mode.
const AdminUser = "admin"

// ErrBadLogin is a wrong password; ErrLocked is too many of them.
var (
	ErrBadLogin = errors.New("wrong password")
	ErrLocked   = errors.New("too many failed sign-ins; try again in a few minutes")
)

// Authenticator resolves callers.
type Authenticator struct {
	cfg     config.Auth
	key     []byte
	now     func() time.Time
	limiter *limiter
}

// New returns an Authenticator for the configured mode.
func New(cfg config.Auth) *Authenticator {
	return &Authenticator{cfg: cfg, key: []byte(cfg.SessionKey), now: time.Now, limiter: newLimiter(10, 15*time.Minute)}
}

// Mode is password or header.
func (a *Authenticator) Mode() string { return a.cfg.Mode }

type session struct {
	User    string `json:"u"`
	Admin   bool   `json:"a"`
	Expires int64  `json:"e"`
}

// Caller returns the authenticated caller, or false.
func (a *Authenticator) Caller(r *http.Request) (sandbox.Caller, bool) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") && a.cfg.APIToken != "" {
		if equal(strings.TrimPrefix(h, "Bearer "), a.cfg.APIToken) {
			return sandbox.Caller{Name: AdminUser, Admin: true}, true
		}
		return sandbox.Caller{}, false
	}
	if a.cfg.Mode == config.AuthHeader {
		user := strings.TrimSpace(r.Header.Get(a.cfg.UserHeader))
		if user == "" || len(user) > 256 {
			return sandbox.Caller{}, false
		}
		admin := false
		for _, u := range a.cfg.AdminUsers {
			if strings.EqualFold(u, user) {
				admin = true
			}
		}
		return sandbox.Caller{Name: user, Admin: admin}, true
	}
	c, err := r.Cookie(CookieName)
	if err != nil {
		return sandbox.Caller{}, false
	}
	s, ok := a.verify(c.Value)
	if !ok {
		return sandbox.Caller{}, false
	}
	return sandbox.Caller{Name: s.User, Admin: s.Admin}, true
}

// Login checks the admin password and sets the session cookie.
func (a *Authenticator) Login(w http.ResponseWriter, r *http.Request, password string) error {
	if a.cfg.Mode != config.AuthPassword {
		return errors.New("sign-in is handled by your identity proxy")
	}
	ip := clientIP(r)
	if a.limiter.blocked(ip, a.now()) {
		return ErrLocked
	}
	if !equal(password, a.cfg.AdminPassword) {
		a.limiter.fail(ip, a.now())
		return ErrBadLogin
	}
	a.limiter.reset(ip)
	exp := a.now().Add(a.cfg.SessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    a.sign(session{User: AdminUser, Admin: true, Expires: exp.Unix()}),
		Path:     "/",
		Expires:  exp,
		HttpOnly: true,
		Secure:   a.cfg.SecureCookies,
		SameSite: http.SameSiteStrictMode,
	})
	return nil
}

// Logout clears the session cookie.
func (a *Authenticator) Logout(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: a.cfg.SecureCookies, SameSite: http.SameSiteStrictMode,
	})
}

func (a *Authenticator) sign(s session) string {
	body, _ := json.Marshal(s)
	payload := base64.RawURLEncoding.EncodeToString(body)
	return payload + "." + base64.RawURLEncoding.EncodeToString(a.mac(payload))
}

func (a *Authenticator) verify(v string) (session, bool) {
	var s session
	payload, sig, ok := strings.Cut(v, ".")
	if !ok {
		return s, false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, a.mac(payload)) {
		return s, false
	}
	body, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || json.Unmarshal(body, &s) != nil {
		return s, false
	}
	if a.now().Unix() >= s.Expires || s.User == "" {
		return s, false
	}
	return s, true
}

func (a *Authenticator) mac(payload string) []byte {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte("vishwakarma-session-v1:"))
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// equal compares secrets in constant time (for equal-length hashes).
func equal(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// clientIP is the peer address. X-Forwarded-For is not trusted: behind an
// ingress every client then shares the proxy address, which makes the
// limiter stricter, never looser.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limiter blocks an address after max failures within window.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, hits: map[string][]time.Time{}}
}

func (l *limiter) prune(key string, now time.Time) []time.Time {
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.hits, key)
		return nil
	}
	l.hits[key] = kept
	return kept
}

func (l *limiter) blocked(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(key, now)) >= l.max
}

func (l *limiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hits[key] = append(l.prune(key, now), now)
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}
