// Package auth handles admin login: bcrypt password verification, random
// session tokens, HttpOnly cookies, and the middleware that gates the
// /admin/* routes. The Svelte UI in PR #4 will replace the bundled HTML
// login form, but the cookie/session contract stays the same.
//
// Unlike bot/pay/web, this package imports store directly. The contract
// it needs is exactly the store's User/Session shape — re-declaring those
// in a parallel hierarchy just for decoupling would be ceremony with no
// payoff, since auth is internal infrastructure, not domain logic.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/OlexiyOdarchuk/monokasa/internal/store"
	"github.com/OlexiyOdarchuk/monokasa/internal/web"
)

// Tunables.
const (
	// SessionCookie is the cookie name carrying the opaque session token.
	SessionCookie = "monokasa_admin"
	// SessionTTL is how long a freshly-issued session stays valid.
	SessionTTL = 30 * 24 * time.Hour
	// bcryptCost ~250ms on a modern laptop — slows brute force without
	// making login feel sluggish.
	bcryptCost = 12
	// tokenBytes drives NewToken's output: 32 raw bytes → 64 hex chars,
	// 256 bits of entropy.
	tokenBytes = 32
)

// Errors surfaced to handlers and middleware.
var (
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrNoSession          = errors.New("no session")
)

// User is the subset of store.User the auth layer exposes via context.
type User struct {
	ID    int64
	Email string
	Name  string
}

// Store is the persistence behavior auth needs. Satisfied by *store.Store.
type Store interface {
	FindUserByEmail(ctx context.Context, email string) (store.User, error)
	CreateSession(ctx context.Context, userID int64, token string, ttl time.Duration) (store.Session, error)
	FindSession(ctx context.Context, token string) (store.Session, store.User, error)
	DeleteSession(ctx context.Context, token string) error
}

// Handler bundles the login/logout endpoints and the middleware.
type Handler struct {
	st            Store
	secureCookies bool // set true when the public origin is HTTPS-only
	limiter       *web.Limiter
}

// NewHandler wires the auth package to a Store and a deployment hint.
// secureCookies should be true in production behind HTTPS; false for
// localhost development.
func NewHandler(s Store, secureCookies bool) *Handler {
	// 1 attempt per 6s sustained, burst 5 — covers a human fat-finger
	// while making online bcrypt brute force infeasible (bcrypt cost
	// 12 ≈ 250ms per check, so 5/6s gives ~50 hours per million tries
	// per IP — already lost cause for the attacker).
	return &Handler{st: s, secureCookies: secureCookies, limiter: web.NewLimiter(1.0/6, 5)}
}

// RunGC trims idle limiter buckets so the map doesn't grow unbounded.
func (h *Handler) RunGC(ctx context.Context, every, idleMax time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			h.limiter.GC(idleMax)
		}
	}
}

// Register attaches the login/logout routes to a mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/admin/login", h.handleLogin)
	mux.HandleFunc("/admin/logout", h.handleLogout)
}

type ctxKey struct{}

// RequireAuth wraps a handler so it only fires when a valid session cookie
// is present. On miss it 303s GET requests to /admin/login and 401s the
// rest — APIs get a clean status, browser navigation gets a redirect.
func (h *Handler) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := h.userFromRequest(r)
		if err != nil {
			if r.Method == http.MethodGet && acceptsHTML(r) {
				http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, u)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// UserFromContext pulls the authenticated user out of a request handled
// downstream of RequireAuth. Returns zero User and false if missing.
func UserFromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(ctxKey{}).(User)
	return u, ok
}

// userFromRequest reads the session cookie and resolves it to a User.
func (h *Handler) userFromRequest(r *http.Request) (User, error) {
	ck, err := r.Cookie(SessionCookie)
	if err != nil {
		return User{}, ErrNoSession
	}
	_, su, err := h.st.FindSession(r.Context(), ck.Value)
	if err != nil {
		return User{}, err
	}
	return User{ID: su.ID, Email: su.Email, Name: su.Name}, nil
}

// HashPassword bcrypts a plaintext password. The cost is fixed at
// package level so a future tune doesn't desync with stored hashes —
// bcrypt verifies regardless of which cost the hash was minted at.
func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// VerifyPassword compares a stored bcrypt hash with a plaintext attempt.
// Returns nil on match, ErrInvalidCredentials on mismatch, or the
// underlying bcrypt error for malformed hashes.
func VerifyPassword(hash, plain string) error {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain))
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return ErrInvalidCredentials
	}
	return err
}

// NewToken returns a 256-bit random session token, hex-encoded.
func NewToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// handleLogin serves the form on GET and verifies credentials on POST.
// On success it mints a session row, sets the cookie, and 303s to /admin.
func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// Already authenticated → straight to /admin so refresh doesn't
		// stick the user on a login form they don't need.
		if _, err := h.userFromRequest(r); err == nil {
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
			return
		}
		writeLoginForm(w, http.StatusOK, "")
	case http.MethodPost:
		if !h.limiter.Allow(web.ClientIP(r)) {
			writeLoginForm(w, http.StatusTooManyRequests,
				"забагато спроб — почекай хвилину")
			return
		}
		if err := r.ParseForm(); err != nil {
			writeLoginForm(w, http.StatusBadRequest, "помилка форми")
			return
		}
		email := strings.TrimSpace(r.PostFormValue("email"))
		password := r.PostFormValue("password")
		if email == "" || password == "" {
			writeLoginForm(w, http.StatusBadRequest, "введи email і пароль")
			return
		}
		u, err := h.st.FindUserByEmail(r.Context(), email)
		if err != nil {
			// Don't distinguish "no such user" from "wrong password" —
			// generic message preserves account enumeration resistance.
			slog.Info("login: user lookup failed", "email", email, "err", err)
			writeLoginForm(w, http.StatusUnauthorized, "невірний email або пароль")
			return
		}
		if err := VerifyPassword(u.PasswordHash, password); err != nil {
			slog.Info("login: bad password", "email", email)
			writeLoginForm(w, http.StatusUnauthorized, "невірний email або пароль")
			return
		}
		tok, err := NewToken()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if _, err := h.st.CreateSession(r.Context(), u.ID, tok, SessionTTL); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		h.setCookie(w, r, tok, SessionTTL)
		slog.Info("login ok", "email", u.Email, "userId", u.ID)
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleLogout revokes the current session row and clears the cookie.
// Idempotent — missing cookie or already-deleted row both return 303.
func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if ck, err := r.Cookie(SessionCookie); err == nil {
		_ = h.st.DeleteSession(r.Context(), ck.Value)
	}
	h.clearCookie(w, r)
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (h *Handler) setCookie(w http.ResponseWriter, r *http.Request, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.secureCookies || isTLS(r),
		MaxAge:   int(ttl.Seconds()),
	})
}

func (h *Handler) clearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.secureCookies || isTLS(r),
		MaxAge:   -1,
	})
}

// ConstantTimeCompare wraps subtle.ConstantTimeCompare with a Go-friendly
// boolean return. Used by any shared-secret check that should resist
// timing attacks (scanner token, future webhook secrets, etc.).
func ConstantTimeCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func isTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// acceptsHTML returns true when the client looks like a browser, so a 303
// to the login page is more useful than a 401 JSON-ish body. Empty Accept
// is treated as "browser" so plain curl also lands on the redirect.
func acceptsHTML(r *http.Request) bool {
	a := r.Header.Get("Accept")
	if a == "" {
		return true
	}
	return strings.Contains(a, "text/html")
}

var loginTmpl = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="uk"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>monokasa · вхід</title>
<style>
  body{font-family:system-ui,sans-serif;background:#111;color:#eee;margin:0;display:grid;place-items:center;height:100vh}
  form{background:#1a1a1a;padding:2rem;border-radius:.75rem;width:min(360px,90vw);box-shadow:0 4px 16px rgba(0,0,0,.4)}
  h1{margin:0 0 1rem;font-size:1.25rem}
  label{display:block;margin-top:.75rem;font-size:.875rem;color:#aaa}
  input{width:100%;box-sizing:border-box;padding:.625rem;background:#0a0a0a;border:1px solid #333;border-radius:.375rem;color:#eee;font-size:1rem}
  input:focus{outline:none;border-color:#888}
  button{margin-top:1.25rem;width:100%;padding:.75rem;background:#d97757;color:#000;border:0;border-radius:.375rem;font-size:1rem;font-weight:600;cursor:pointer}
  button:hover{background:#e88865}
  .err{margin-top:.75rem;padding:.625rem;background:#3a1a1a;border:1px solid #5a2a2a;border-radius:.375rem;color:#fbb;font-size:.875rem}
</style>
</head><body>
<form method="post" action="/admin/login">
  <h1>monokasa · вхід для адміна</h1>
  <label>Email</label>
  <input type="email" name="email" required autofocus>
  <label>Пароль</label>
  <input type="password" name="password" required>
  {{if .Err}}<div class="err">{{.Err}}</div>{{end}}
  <button type="submit">Увійти</button>
</form>
</body></html>`))

func writeLoginForm(w http.ResponseWriter, status int, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := loginTmpl.Execute(w, map[string]string{"Err": errMsg}); err != nil {
		fmt.Fprintln(w, "<!-- template error -->")
	}
}
