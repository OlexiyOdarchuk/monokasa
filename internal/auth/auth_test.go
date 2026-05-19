package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OlexiyOdarchuk/monokasa/internal/auth"
	"github.com/OlexiyOdarchuk/monokasa/internal/store"
)

// newStore opens a fresh on-disk SQLite for the test — exercises the same
// driver and schema the production binary uses, so we don't ship a fake
// adapter that drifts from reality.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "tix.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedAdmin(t *testing.T, s *store.Store, email, password string) store.User {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	u, err := s.CreateUser(context.Background(), email, "Admin", hash)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}

func TestPasswordRoundtrip(t *testing.T) {
	hash, err := auth.HashPassword("p@ssw0rd")
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.VerifyPassword(hash, "p@ssw0rd"); err != nil {
		t.Errorf("correct password rejected: %v", err)
	}
	if err := auth.VerifyPassword(hash, "wrong"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("wrong password: got %v, want ErrInvalidCredentials", err)
	}
}

func TestNewTokenUnique(t *testing.T) {
	seen := make(map[string]struct{})
	for i := range 100 {
		tok, err := auth.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) != 64 { // 32 bytes hex
			t.Errorf("token len = %d, want 64", len(tok))
		}
		if _, dup := seen[tok]; dup {
			t.Fatalf("token collision after %d iterations", i)
		}
		seen[tok] = struct{}{}
	}
}

func TestLoginHappyPath(t *testing.T) {
	s := newStore(t)
	seedAdmin(t, s, "admin@example.com", "p@ssw0rd")

	h := auth.NewHandler(s, false)
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	form := url.Values{"email": {"admin@example.com"}, "password": {"p@ssw0rd"}}
	resp := postForm(t, srv, "/admin/login", form, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (See Other)", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin" {
		t.Errorf("Location = %q, want /admin", loc)
	}
	ck := pickCookie(resp, auth.SessionCookie)
	if ck == nil || ck.Value == "" {
		t.Fatalf("no %s cookie set", auth.SessionCookie)
	}
	if !ck.HttpOnly {
		t.Errorf("session cookie should be HttpOnly")
	}
	if ck.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie SameSite = %v, want Lax", ck.SameSite)
	}
}

func TestLoginBadPasswordRejected(t *testing.T) {
	s := newStore(t)
	seedAdmin(t, s, "admin@example.com", "right")

	h := auth.NewHandler(s, false)
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	form := url.Values{"email": {"admin@example.com"}, "password": {"wrong"}}
	resp := postForm(t, srv, "/admin/login", form, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if pickCookie(resp, auth.SessionCookie) != nil {
		t.Error("bad login should not set session cookie")
	}
}

func TestLoginUnknownEmailRejected(t *testing.T) {
	s := newStore(t)
	h := auth.NewHandler(s, false)
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	form := url.Values{"email": {"nobody@example.com"}, "password": {"anything"}}
	resp := postForm(t, srv, "/admin/login", form, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestRequireAuthRejectsAnonymous(t *testing.T) {
	s := newStore(t)
	h := auth.NewHandler(s, false)

	protected := h.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok := auth.UserFromContext(r.Context())
		if !ok {
			t.Error("RequireAuth let through without user in context")
		}
		_, _ = w.Write([]byte("ok"))
	}))
	mux := http.NewServeMux()
	mux.Handle("/admin", protected)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// API-style request (no Accept: text/html) → 401.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/admin", nil)
	req.Header.Set("Accept", "application/json")
	client := noRedirectClient()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("API-style miss: status = %d, want 401", resp.StatusCode)
	}

	// Browser-style request → 303 to /admin/login.
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/admin", nil)
	req2.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther {
		t.Errorf("browser miss: status = %d, want 303", resp2.StatusCode)
	}
}

func TestRequireAuthAcceptsValidSession(t *testing.T) {
	s := newStore(t)
	u := seedAdmin(t, s, "admin@example.com", "secret")
	tok, _ := auth.NewToken()
	if _, err := s.CreateSession(context.Background(), u.ID, tok, auth.SessionTTL); err != nil {
		t.Fatal(err)
	}

	h := auth.NewHandler(s, false)
	var sawUser auth.User
	protected := h.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := auth.UserFromContext(r.Context())
		if !ok {
			t.Error("missing user in context")
		}
		sawUser = got
		_, _ = w.Write([]byte("ok"))
	}))
	mux := http.NewServeMux()
	mux.Handle("/admin", protected)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/admin", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: tok})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if sawUser.Email != "admin@example.com" || sawUser.ID != u.ID {
		t.Errorf("sawUser = %+v, want {ID: %d, Email: admin@example.com}", sawUser, u.ID)
	}
}

func TestRequireAuthRejectsExpiredSession(t *testing.T) {
	s := newStore(t)
	u := seedAdmin(t, s, "admin@example.com", "secret")
	tok, _ := auth.NewToken()
	if _, err := s.CreateSession(context.Background(), u.ID, tok, -time.Minute); err != nil {
		t.Fatal(err)
	}

	h := auth.NewHandler(s, false)
	protected := h.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("expired session should not reach handler")
	}))
	mux := http.NewServeMux()
	mux.Handle("/admin", protected)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/admin", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: tok})
	req.Header.Set("Accept", "application/json")
	client := noRedirectClient()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expired: status = %d, want 401", resp.StatusCode)
	}
}

func TestLogoutClearsSessionAndCookie(t *testing.T) {
	s := newStore(t)
	u := seedAdmin(t, s, "admin@example.com", "secret")
	tok, _ := auth.NewToken()
	if _, err := s.CreateSession(context.Background(), u.ID, tok, auth.SessionTTL); err != nil {
		t.Fatal(err)
	}

	h := auth.NewHandler(s, false)
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/logout", strings.NewReader(""))
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: tok})
	client := noRedirectClient()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("logout status = %d, want 303", resp.StatusCode)
	}
	ck := pickCookie(resp, auth.SessionCookie)
	if ck == nil || ck.MaxAge >= 0 {
		t.Errorf("logout should clear cookie (MaxAge < 0), got %+v", ck)
	}
	if _, _, err := s.FindSession(context.Background(), tok); !errors.Is(err, store.ErrSessionNotFound) {
		t.Errorf("session row should be gone, got %v", err)
	}
}

// --- helpers ---

func postForm(t *testing.T, srv *httptest.Server, path string, form url.Values, cookies []*http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func pickCookie(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
