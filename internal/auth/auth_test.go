package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dmdhrumilmistry/vishwakarma/internal/config"
)

func passwordAuth() *Authenticator {
	return New(config.Auth{
		Mode:          config.AuthPassword,
		AdminPassword: "correct-horse-battery",
		APIToken:      "token-token-token-token-token",
		SessionKey:    strings.Repeat("k", 32),
		SessionTTL:    time.Hour,
		SecureCookies: true,
	})
}

func login(t *testing.T, a *Authenticator, pw string) (*http.Cookie, error) {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/v1/login", nil)
	err := a.Login(w, r, pw)
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName {
			return c, err
		}
	}
	return nil, err
}

func withCookie(c *http.Cookie) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(c)
	return r
}

func TestSessionRoundTrip(t *testing.T) {
	a := passwordAuth()
	c, err := login(t, a, "correct-horse-battery")
	if err != nil || c == nil {
		t.Fatalf("login: %v", err)
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie flags %+v", c)
	}
	caller, ok := a.Caller(withCookie(c))
	if !ok || caller.Name != AdminUser || !caller.Admin {
		t.Fatalf("caller %+v %v", caller, ok)
	}
}

func TestSessionTamperAndExpiry(t *testing.T) {
	a := passwordAuth()
	c, _ := login(t, a, "correct-horse-battery")

	forged := *c
	payload, sig, _ := strings.Cut(c.Value, ".")
	forged.Value = payload + "x." + sig
	if _, ok := a.Caller(withCookie(&forged)); ok {
		t.Error("tampered payload accepted")
	}

	other := passwordAuth()
	other.key = []byte(strings.Repeat("z", 32))
	if _, ok := other.Caller(withCookie(c)); ok {
		t.Error("cookie signed with another key accepted")
	}

	a.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, ok := a.Caller(withCookie(c)); ok {
		t.Error("expired session accepted")
	}
}

func TestBearerToken(t *testing.T) {
	a := passwordAuth()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer token-token-token-token-token")
	if c, ok := a.Caller(r); !ok || !c.Admin {
		t.Fatalf("valid token rejected: %+v", c)
	}
	r.Header.Set("Authorization", "Bearer wrong")
	if _, ok := a.Caller(r); ok {
		t.Error("wrong token accepted")
	}
}

func TestLoginLockout(t *testing.T) {
	a := passwordAuth()
	for i := 0; i < 10; i++ {
		if _, err := login(t, a, "nope"); err != ErrBadLogin {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := login(t, a, "correct-horse-battery"); err != ErrLocked {
		t.Fatalf("after 10 failures: %v, want ErrLocked even with the right password", err)
	}
	a.now = func() time.Time { return time.Now().Add(16 * time.Minute) }
	if _, err := login(t, a, "correct-horse-battery"); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

func TestHeaderMode(t *testing.T) {
	a := New(config.Auth{Mode: config.AuthHeader, UserHeader: "X-Forwarded-User", AdminUsers: []string{"Root@Example.com"}, SessionKey: strings.Repeat("k", 32)})
	r := httptest.NewRequest("GET", "/", nil)
	if _, ok := a.Caller(r); ok {
		t.Error("no header must mean no caller")
	}
	r.Header.Set("X-Forwarded-User", "dev@example.com")
	if c, ok := a.Caller(r); !ok || c.Name != "dev@example.com" || c.Admin {
		t.Errorf("user %+v", c)
	}
	r.Header.Set("X-Forwarded-User", "root@example.com")
	if c, _ := a.Caller(r); !c.Admin {
		t.Error("admin users match case-insensitively")
	}
	if err := a.Login(httptest.NewRecorder(), r, "x"); err == nil {
		t.Error("password login must be disabled in header mode")
	}
}
