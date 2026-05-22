// Package web hosts the entrance-side QR scanner page and check endpoint.
package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/OlexiyOdarchuk/monokasa/internal/metrics"
)

// Seat is the subset of seat info shown next to a scanned ticket.
type Seat struct {
	ID       int64
	Row      int
	Col      int
	Category string
}

// Reservation is the subset of reservation info shown next to a scanned ticket.
type Reservation struct {
	BuyerName   string
	ConfirmedAt *time.Time
}

// Ticket is the subset of ticket info the scanner needs.
type Ticket struct {
	ID     int64
	UsedAt *time.Time
}

// Domain errors the scanner expects the Store to return.
var (
	ErrTicketNotFound = errors.New("ticket not found")
	ErrTicketUsed     = errors.New("ticket already used")
)

// Store is the persistence behavior the scanner needs.
type Store interface {
	UseTicket(ctx context.Context, qrPayload string) (Ticket, error)
	FindReservationByTicket(ctx context.Context, ticketID int64) (Reservation, Seat, error)
}

// Coder verifies a QR payload signature.
type Coder interface {
	VerifyQRPayload(payload string) (reservationID, seatID int64, err error)
}

// Scanner is the QR-scanner web app: GET /scan serves the HTML, POST
// /scan/check validates a payload and marks the ticket as used.
type Scanner struct {
	store    Store
	coder    Coder
	token    string // shared auth token; empty disables auth
	botToken string // Telegram bot token; enables WebApp initData auth
	adminTG  map[int64]bool
	limiter  *Limiter
}

func NewScanner(s Store, c Coder, authToken string) *Scanner {
	// 10 req/s sustained, burst 20 — a doorman can comfortably scan one
	// QR per second; anything faster is either an automated probe or a
	// stuck client. Per-IP, so multiple staff devices don't fight.
	return &Scanner{store: s, coder: c, token: authToken, limiter: NewLimiter(10, 20)}
}

// EnableTelegramWebApp turns on the second auth path: scanners opened
// as Telegram Mini Apps can authenticate via initData instead of the
// shared password cookie. adminTGIDs is the allow-list — only those
// Telegram users get through, even if initData verifies.
func (s *Scanner) EnableTelegramWebApp(botToken string, adminTGIDs []int64) {
	s.botToken = botToken
	s.adminTG = make(map[int64]bool, len(adminTGIDs))
	for _, id := range adminTGIDs {
		if id != 0 {
			s.adminTG[id] = true
		}
	}
}

func (s *Scanner) Register(mux *http.ServeMux) {
	mux.HandleFunc("/scan", s.handlePage)
	mux.HandleFunc("/scan/check", s.handleCheck)
}

// RunGC periodically drops idle rate-limit buckets. Call from main with
// the lifecycle context; returns when ctx is cancelled.
func (s *Scanner) RunGC(ctx context.Context, every, idleMax time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.limiter.GC(idleMax)
		}
	}
}

// scanCookie holds the validated SCANNER_TOKEN after a successful login.
// HttpOnly + Path=/scan + 12h MaxAge — long enough to cover a single
// performance shift, short enough that a lost device stops working by
// next day.
const scanCookie = "monokasa_scan"
const scanCookieMaxAge = 12 * 60 * 60

// authOK checks cookie first, then the X-Scanner-Token header (curl path).
// The password form at GET/POST /scan is the only way to obtain the cookie.
func (s *Scanner) authOK(r *http.Request) bool {
	if s.token == "" {
		return true
	}
	if ck, err := r.Cookie(scanCookie); err == nil {
		if subtle.ConstantTimeCompare([]byte(ck.Value), []byte(s.token)) == 1 {
			return true
		}
	}
	if hdr := r.Header.Get("X-Scanner-Token"); hdr != "" {
		if subtle.ConstantTimeCompare([]byte(hdr), []byte(s.token)) == 1 {
			return true
		}
	}
	// Telegram WebApp auth: header X-Telegram-Init-Data is the
	// signed initData blob the Mini App SDK exposes. We verify the
	// signature against the bot token and check the user against the
	// admin allow-list. Bot token isn't a secret known to attackers
	// (lives in the server's env), so this is a real second factor.
	if s.botToken != "" {
		if raw := r.Header.Get("X-Telegram-Init-Data"); raw != "" {
			userID, err := VerifyTelegramInitData(raw, s.botToken)
			if err == nil && s.adminTG[userID] {
				return true
			}
		}
	}
	return false
}

// handlePage is a single endpoint that serves either the scanner UI or
// the password gate, depending on whether SCANNER_TOKEN is set and whether
// the request has a valid cookie. POST is the form submit; on success it
// sets the cookie and 303s back to GET /scan, which now lands on the UI.
func (s *Scanner) handlePage(w http.ResponseWriter, r *http.Request) {
	// No password configured → scanner is open. Only GET makes sense.
	if s.token == "" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeScannerPage(w)
		return
	}

	switch r.Method {
	case http.MethodGet:
		// Mini App entry: ?tg=1 query opts into the Telegram WebApp
		// flow. The page itself exposes no data; real auth happens on
		// /scan/check, which validates the initData header set by JS.
		if s.botToken != "" && r.URL.Query().Get("tg") == "1" {
			writeScannerPage(w)
			return
		}
		if s.authOK(r) {
			writeScannerPage(w)
			return
		}
		writeLoginPage(w, http.StatusOK, "")
	case http.MethodPost:
		// Login attempts share the same per-IP token bucket as /scan/check,
		// so a password brute-force gets throttled to ~10 attempts/s.
		if !s.limiter.Allow(ClientIP(r)) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		if err := r.ParseForm(); err != nil {
			writeLoginPage(w, http.StatusBadRequest, "помилка форми")
			return
		}
		pw := r.PostFormValue("password")
		if subtle.ConstantTimeCompare([]byte(pw), []byte(s.token)) != 1 {
			writeLoginPage(w, http.StatusUnauthorized, "невірний пароль")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     scanCookie,
			Value:    s.token,
			Path:     "/scan",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   isTLS(r),
			MaxAge:   scanCookieMaxAge,
		})
		http.Redirect(w, r, "/scan", http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func writeScannerPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(pageHTML))
}

func writeLoginPage(w http.ResponseWriter, status int, errMsg string) {
	body := loginHTML
	if errMsg != "" {
		body = strings.Replace(body, "{{ERR}}", `<div class="err">`+errMsg+`</div>`, 1)
	} else {
		body = strings.Replace(body, "{{ERR}}", "", 1)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// isTLS returns true when the request reached us over HTTPS, either
// directly or via a reverse proxy honouring X-Forwarded-Proto.
func isTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

type checkRequest struct {
	Payload string `json:"payload"`
}

type checkResponse struct {
	Status   string `json:"status"` // ok | used | invalid
	Detail   string `json:"detail,omitempty"`
	Buyer    string `json:"buyer,omitempty"`
	Seat     string `json:"seat,omitempty"`
	BookedAt string `json:"bookedAt,omitempty"`
	UsedAt   string `json:"usedAt,omitempty"`
}

func (s *Scanner) handleCheck(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !s.limiter.Allow(ClientIP(r)) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	var req checkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, checkResponse{Status: "invalid", Detail: "malformed body"})
		return
	}
	payload := strings.TrimSpace(req.Payload)
	if payload == "" {
		writeJSON(w, http.StatusBadRequest, checkResponse{Status: "invalid", Detail: "empty payload"})
		return
	}
	if _, _, err := s.coder.VerifyQRPayload(payload); err != nil {
		metrics.ScansInvalid.Add(1)
		writeJSON(w, http.StatusOK, checkResponse{Status: "invalid", Detail: err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	t, err := s.store.UseTicket(ctx, payload)
	switch {
	case errors.Is(err, ErrTicketUsed):
		metrics.ScansUsed.Add(1)
		res, seat, _ := s.store.FindReservationByTicket(ctx, t.ID)
		writeJSON(w, http.StatusOK, checkResponse{
			Status:   "used",
			Buyer:    res.BuyerName,
			Seat:     seatLabel(seat),
			BookedAt: bookedAt(res.ConfirmedAt),
			UsedAt:   formatDateTime(*t.UsedAt),
		})
		return
	case errors.Is(err, ErrTicketNotFound):
		metrics.ScansInvalid.Add(1)
		writeJSON(w, http.StatusOK, checkResponse{Status: "invalid", Detail: "ticket not found"})
		return
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, checkResponse{Status: "invalid", Detail: err.Error()})
		return
	}
	metrics.ScansOK.Add(1)
	res, seat, _ := s.store.FindReservationByTicket(ctx, t.ID)
	writeJSON(w, http.StatusOK, checkResponse{
		Status:   "ok",
		Buyer:    res.BuyerName,
		Seat:     seatLabel(seat),
		BookedAt: bookedAt(res.ConfirmedAt),
	})
}

func bookedAt(t *time.Time) string {
	if t == nil {
		return ""
	}
	return formatDateTime(*t)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func seatLabel(s Seat) string {
	if s.ID == 0 {
		return ""
	}
	// GA tickets have no real row/col — render "Вхід · квиток №N" so
	// the doorman sees the same label that printed on the PDF.
	if s.Category == "GA" {
		return fmt.Sprintf("Вхід · квиток №%d", s.Col)
	}
	return fmt.Sprintf("Ряд %d · місце %d", s.Row, s.Col)
}

var ukMonthsGenitive = [...]string{
	"січня", "лютого", "березня", "квітня", "травня", "червня",
	"липня", "серпня", "вересня", "жовтня", "листопада", "грудня",
}

func formatDateTime(t time.Time) string {
	return fmt.Sprintf("%d %s %d · %s",
		t.Day(), ukMonthsGenitive[t.Month()-1], t.Year(), t.Format("15:04"))
}
