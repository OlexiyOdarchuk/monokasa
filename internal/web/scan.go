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
)

// Seat is the subset of seat info shown next to a scanned ticket.
type Seat struct {
	ID  int64
	Row int
	Col int
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
	store Store
	coder Coder
	token string // shared auth token; empty disables auth
}

func NewScanner(s Store, c Coder, authToken string) *Scanner {
	return &Scanner{store: s, coder: c, token: authToken}
}

func (s *Scanner) Register(mux *http.ServeMux) {
	mux.HandleFunc("/scan", s.handlePage)
	mux.HandleFunc("/scan/check", s.handleCheck)
}

func (s *Scanner) authOK(r *http.Request) bool {
	if s.token == "" {
		return true
	}
	got := r.URL.Query().Get("token")
	if got == "" {
		got = r.Header.Get("X-Scanner-Token")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

func (s *Scanner) handlePage(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(pageHTML))
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
		writeJSON(w, http.StatusOK, checkResponse{Status: "invalid", Detail: err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	t, err := s.store.UseTicket(ctx, payload)
	switch {
	case errors.Is(err, ErrTicketUsed):
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
		writeJSON(w, http.StatusOK, checkResponse{Status: "invalid", Detail: "ticket not found"})
		return
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, checkResponse{Status: "invalid", Detail: err.Error()})
		return
	}
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
