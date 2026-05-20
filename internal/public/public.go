// Package public exposes the buyer-side JSON API: what's at /event/<slug>,
// what seats are still available, and the POST that creates a reservation
// for a web buyer (no Telegram involved).
//
// Endpoints land under /api/public/* — *no* auth required. Care has been
// taken not to surface admin-only data (tg_user_id, internal IDs unrelated
// to the seat map, full reservation history).
package public

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OlexiyOdarchuk/monokasa/internal/store"
	"github.com/OlexiyOdarchuk/monokasa/internal/token"
)

// Handler is the buyer-side API surface.
type Handler struct {
	st          *store.Store
	coder       *token.Coder
	jarLink     string // monobank jar URL; pay link is jarLink?a=…&t=…
	hold        time.Duration
	priceMin    int64  // minimum price kopecks; reservations below this are rejected
	botUsername string // optional; when set, ReservationResponse carries a TG deep link
}

type Config struct {
	Store       *store.Store
	Coder       *token.Coder
	JarLink     string
	Hold        time.Duration
	MinPrice    int64
	BotUsername string // optional Telegram bot @-handle (no leading "@")
}

func NewHandler(c Config) *Handler {
	return &Handler{
		st:          c.Store,
		coder:       c.Coder,
		jarLink:     c.JarLink,
		hold:        c.Hold,
		priceMin:    c.MinPrice,
		botUsername: c.BotUsername,
	}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/public/shows", h.listShows)
	mux.HandleFunc("GET /api/public/shows/{slug}", h.getShow)
	mux.HandleFunc("POST /api/public/reservations", h.createReservation)
}

// --- GET /api/public/shows ---

type publicShowSummary struct {
	Slug      string    `json:"slug"`
	Title     string    `json:"title"`
	Venue     string    `json:"venue"`
	StartsAt  time.Time `json:"starts_at"`
	SeatsFree int       `json:"seats_free"`
	SeatsTotal int      `json:"seats_total"`
}

func (h *Handler) listShows(w http.ResponseWriter, r *http.Request) {
	shows, err := h.st.ListShows(r.Context())
	if err != nil {
		writeInternal(w, "list shows", err)
		return
	}
	now := time.Now()
	out := make([]publicShowSummary, 0, len(shows))
	for _, sh := range shows {
		// Hide archived shows and ones that already happened more than
		// 2h ago — landing page is forward-looking.
		if sh.ArchivedAt != nil {
			continue
		}
		if sh.StartsAt.Before(now.Add(-2 * time.Hour)) {
			continue
		}
		st, err := h.st.Stats(r.Context(), sh.ID)
		if err != nil {
			// One bad stats query shouldn't blank the whole list.
			slog.Warn("public list: stats failed", "showId", sh.ID, "err", err)
			continue
		}
		out = append(out, publicShowSummary{
			Slug: sh.Slug, Title: sh.Title, Venue: sh.Venue,
			StartsAt:   sh.StartsAt,
			SeatsFree:  st.Free,
			SeatsTotal: st.Total,
		})
	}
	// Sort by start, soonest first.
	sort.Slice(out, func(i, j int) bool { return out[i].StartsAt.Before(out[j].StartsAt) })
	writeJSON(w, http.StatusOK, out)
}

// --- GET /api/public/shows/{slug} ---

type publicShow struct {
	Slug     string      `json:"slug"`
	Title    string      `json:"title"`
	Venue    string      `json:"venue"`
	StartsAt time.Time   `json:"starts_at"`
	Seats    []publicSeat `json:"seats"`
}

type publicSeat struct {
	ID           int64   `json:"id"`
	Row          int     `json:"row"`
	Col          int     `json:"col"`
	X            float64 `json:"x"`
	Y            float64 `json:"y"`
	Label        string  `json:"label"`
	Category     string  `json:"category"`
	PriceKopecks int64   `json:"price_kopecks"`
	Sellable     bool    `json:"sellable"`
	Taken        bool    `json:"taken"` // confirmed OR held — buyer can't grab it
}

func (h *Handler) getShow(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" {
		writeError(w, http.StatusBadRequest, "invalid_slug", "")
		return
	}
	show, err := h.st.LoadShowBySlug(r.Context(), slug)
	if errors.Is(err, store.ErrShowNotFound) {
		writeError(w, http.StatusNotFound, "show_not_found", "")
		return
	}
	if err != nil {
		writeInternal(w, "load show by slug", err)
		return
	}

	seats, err := h.st.Seats(r.Context(), show.ID)
	if err != nil {
		writeInternal(w, "list seats", err)
		return
	}
	statuses, err := h.st.SeatStatuses(r.Context(), show.ID)
	if err != nil {
		writeInternal(w, "seat statuses", err)
		return
	}

	out := publicShow{
		Slug: show.Slug, Title: show.Title, Venue: show.Venue,
		StartsAt: show.StartsAt,
		Seats:    make([]publicSeat, 0, len(seats)),
	}
	for _, s := range seats {
		taken := statuses[s.ID] != store.SeatFree
		out.Seats = append(out.Seats, publicSeat{
			ID: s.ID, Row: s.Row, Col: s.Col,
			X: s.X, Y: s.Y, Label: s.Label, Category: s.Category,
			PriceKopecks: s.PriceKopecks, Sellable: s.Sellable,
			Taken: taken,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// --- POST /api/public/reservations ---

type createReservationRequest struct {
	Slug       string `json:"slug"`
	SeatID     int64  `json:"seat_id"`
	BuyerName  string `json:"buyer_name"`
	BuyerEmail string `json:"buyer_email"`
}

type createReservationResponse struct {
	Code       string     `json:"code"`
	ExpiresAt  time.Time  `json:"expires_at"`
	PayURL     string     `json:"pay_url"`
	Seat       publicSeat `json:"seat"`
	BuyerName  string     `json:"buyer_name"`
	BuyerEmail string     `json:"buyer_email"`
	// TGDeepLink is t.me/<bot>?start=res_<code> when the host has a
	// BOT_USERNAME configured. Empty otherwise — frontend hides the
	// "Connect Telegram" button when this is missing.
	TGDeepLink string `json:"tg_deep_link,omitempty"`
}

func (h *Handler) createReservation(w http.ResponseWriter, r *http.Request) {
	var req createReservationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	name, err := normalizeName(req.BuyerName)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_name", err.Error())
		return
	}
	email, err := normalizeEmail(req.BuyerEmail)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_email", err.Error())
		return
	}
	if req.SeatID <= 0 || req.Slug == "" {
		writeError(w, http.StatusBadRequest, "invalid_input", "slug and seat_id required")
		return
	}

	// Resolve show by slug first — proves the slug exists and isn't
	// archived before we touch the seat.
	show, err := h.st.LoadShowBySlug(r.Context(), req.Slug)
	if errors.Is(err, store.ErrShowNotFound) {
		writeError(w, http.StatusNotFound, "show_not_found", "")
		return
	}
	if err != nil {
		writeInternal(w, "load show", err)
		return
	}

	// Fetch the seat and confirm it belongs to that show — otherwise a
	// stale seat_id from another event could create a cross-show booking.
	seat, err := h.st.Seats(r.Context(), show.ID)
	if err != nil {
		writeInternal(w, "list seats", err)
		return
	}
	var target store.Seat
	var found bool
	for _, s := range seat {
		if s.ID == req.SeatID {
			target = s
			found = true
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "seat_not_found", "seat does not belong to this show")
		return
	}
	if !target.Sellable {
		writeError(w, http.StatusConflict, "seat_not_sellable", "")
		return
	}
	if target.PriceKopecks < h.priceMin {
		// Sanity check — a seat priced below MinPrice would force
		// pay.Processor to reject any matching transaction.
		writeInternal(w, "seat below min price", fmt.Errorf("seat %d price %d < min %d", target.ID, target.PriceKopecks, h.priceMin))
		return
	}

	code, err := h.coder.NewCode()
	if err != nil {
		writeInternal(w, "mint code", err)
		return
	}

	res, err := h.st.Reserve(r.Context(), target, 0, 0, name, email, code, h.hold)
	switch {
	case errors.Is(err, store.ErrSeatTaken):
		writeError(w, http.StatusConflict, "seat_taken", "")
		return
	case errors.Is(err, store.ErrSeatNotSellable):
		writeError(w, http.StatusConflict, "seat_not_sellable", "")
		return
	case err != nil:
		writeInternal(w, "reserve", err)
		return
	}

	payURL := jarPrefillURL(h.jarLink, target.PriceKopecks, res.Code)
	slog.Info("public reservation created",
		"code", res.Code, "slug", show.Slug, "seatId", target.ID,
		"buyer", name, "email", email)

	var tgLink string
	if h.botUsername != "" {
		tgLink = fmt.Sprintf("https://t.me/%s?start=res_%s", h.botUsername, res.Code)
	}

	writeJSON(w, http.StatusCreated, createReservationResponse{
		Code: res.Code, ExpiresAt: res.ExpiresAt,
		PayURL: payURL,
		Seat: publicSeat{
			ID: target.ID, Row: target.Row, Col: target.Col,
			X: target.X, Y: target.Y, Label: target.Label, Category: target.Category,
			PriceKopecks: target.PriceKopecks, Sellable: true,
		},
		BuyerName:  name,
		BuyerEmail: email,
		TGDeepLink: tgLink,
	})
}

// --- helpers ---

const buyerNameMaxRunes = 60

// normalizeName mirrors what the bot accepts: trims and collapses spaces,
// enforces 2..60 runes. Refuses obviously empty values.
func normalizeName(in string) (string, error) {
	n := strings.Join(strings.Fields(in), " ")
	rc := utf8.RuneCountInString(n)
	if rc < 2 {
		return "", fmt.Errorf("name must be at least 2 characters")
	}
	if rc > buyerNameMaxRunes {
		return "", fmt.Errorf("name too long (max %d chars)", buyerNameMaxRunes)
	}
	return n, nil
}

// normalizeEmail parses with net/mail.ParseAddress so we accept whatever
// RFC 5322 considers valid, then strips the display name and lowercases
// the domain (keeps the local-part as-typed — case-sensitive servers exist).
func normalizeEmail(in string) (string, error) {
	in = strings.TrimSpace(in)
	if in == "" {
		return "", fmt.Errorf("email is required")
	}
	addr, err := mail.ParseAddress(in)
	if err != nil {
		return "", fmt.Errorf("invalid email")
	}
	at := strings.LastIndex(addr.Address, "@")
	if at < 0 {
		return "", fmt.Errorf("invalid email")
	}
	return addr.Address[:at] + "@" + strings.ToLower(addr.Address[at+1:]), nil
}

// jarPrefillURL mirrors the bot-side helper (kept in lockstep): appends
// ?a=<UAH>&t=<code> to the jar link so monobank opens with amount and
// comment pre-filled.
func jarPrefillURL(base string, priceKopecks int64, comment string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	major := priceKopecks / 100
	minor := priceKopecks % 100
	var amount string
	if minor == 0 {
		amount = strconv.FormatInt(major, 10)
	} else {
		amount = fmt.Sprintf("%d.%02d", major, minor)
	}
	q := u.Query()
	q.Set("a", amount)
	q.Set("t", comment)
	u.RawQuery = q.Encode()
	return u.String()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, map[string]string{"error": code, "detail": detail})
}

func writeInternal(w http.ResponseWriter, op string, err error) {
	slog.Error("public: "+op, "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "")
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}

