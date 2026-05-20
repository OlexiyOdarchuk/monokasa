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
	mux.HandleFunc("POST /api/public/orders", h.createOrder)
	mux.HandleFunc("GET /api/public/reservations/{code}/status", h.reservationStatus)
}

// --- GET /api/public/reservations/{code}/status ---
//
// Public endpoint for the success screen on /event/<slug> to poll until
// the buyer's payment lands. Returns only the status enum — no buyer
// name, email or chat id — so the link can be shared without leaking
// PII to anyone with the code (codes are 8-char base32, ~40 bits, fine
// to leave un-rate-limited).

type reservationStatusResponse struct {
	Status string `json:"status"` // held | paid | expired | cancelled
}

func (h *Handler) reservationStatus(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if code == "" {
		writeError(w, http.StatusBadRequest, "invalid_code", "")
		return
	}
	// Orders carry the bare 8-char base32 code that the buyer pastes into
	// the monobank comment. For multi-seat orders each child reservation
	// has a derived "<code>.<seq>" id, so the order row is the only place
	// where the unified status lives. Single-seat orders also live here
	// (Reserve delegates to CreateOrder([seat]) under the hood), so this
	// branch covers both cases.
	order, _, err := h.st.FindOrderByCode(r.Context(), code)
	switch {
	case errors.Is(err, store.ErrCodeNotFound):
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	case errors.Is(err, store.ErrAlreadyClosed):
		writeJSON(w, http.StatusOK, reservationStatusResponse{Status: "cancelled"})
		return
	case err != nil:
		writeInternal(w, "order status", err)
		return
	}
	status := "held"
	switch {
	case order.ConfirmedAt != nil:
		status = "paid"
	case order.ExpiresAt.Before(time.Now()):
		status = "expired"
	}
	writeJSON(w, http.StatusOK, reservationStatusResponse{Status: status})
}

// --- GET /api/public/shows ---

type publicShowSummary struct {
	Slug        string    `json:"slug"`
	Title       string    `json:"title"`
	Venue       string    `json:"venue"`
	StartsAt    time.Time `json:"starts_at"`
	Description string    `json:"description"`
	PosterURL   string    `json:"poster_url"`
	SeatsFree   int       `json:"seats_free"`
	SeatsTotal  int       `json:"seats_total"`
}

func (h *Handler) listShows(w http.ResponseWriter, r *http.Request) {
	shows, err := h.st.ListShows(r.Context())
	if err != nil {
		writeInternal(w, "list shows", err)
		return
	}
	out := make([]publicShowSummary, 0, len(shows))
	for _, sh := range shows {
		// Hide archived shows — admin controls visibility via the
		// Archive button in the show editor. We intentionally do NOT
		// filter by start_at: in self-host mode the admin might create
		// a past-dated event for testing and then wonder why it's
		// invisible. If they want it hidden, they archive it.
		if sh.ArchivedAt != nil {
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
			StartsAt:    sh.StartsAt,
			Description: sh.Description,
			PosterURL:   sh.PosterURL,
			SeatsFree:   st.Free,
			SeatsTotal:  st.Total,
		})
	}
	// Sort by start, soonest first.
	sort.Slice(out, func(i, j int) bool { return out[i].StartsAt.Before(out[j].StartsAt) })
	writeJSON(w, http.StatusOK, out)
}

// --- GET /api/public/shows/{slug} ---

type publicShow struct {
	Slug        string       `json:"slug"`
	Title       string       `json:"title"`
	Venue       string       `json:"venue"`
	StartsAt    time.Time    `json:"starts_at"`
	Description string       `json:"description"`
	PosterURL   string       `json:"poster_url"`
	Seats       []publicSeat `json:"seats"`
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
		StartsAt:    show.StartsAt,
		Description: show.Description,
		PosterURL:   show.PosterURL,
		Seats:       make([]publicSeat, 0, len(seats)),
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

	order, _, err := h.st.CreateOrder(r.Context(), []store.Seat{target}, 0, 0, name, email, code, h.hold)
	switch {
	case errors.Is(err, store.ErrSeatTaken):
		writeError(w, http.StatusConflict, "seat_taken", "")
		return
	case errors.Is(err, store.ErrSeatNotSellable):
		writeError(w, http.StatusConflict, "seat_not_sellable", "")
		return
	case err != nil:
		writeInternal(w, "create order", err)
		return
	}

	payURL := jarPrefillURL(h.jarLink, target.PriceKopecks, order.Code)
	slog.Info("public reservation created",
		"code", order.Code, "slug", show.Slug, "seatId", target.ID,
		"buyer", name, "email", email)

	var tgLink string
	if h.botUsername != "" {
		tgLink = fmt.Sprintf("https://t.me/%s?start=res_%s", h.botUsername, order.Code)
	}

	writeJSON(w, http.StatusCreated, createReservationResponse{
		Code: order.Code, ExpiresAt: order.ExpiresAt,
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

// --- POST /api/public/orders (multi-seat) ---

type createOrderRequest struct {
	Slug       string  `json:"slug"`
	SeatIDs    []int64 `json:"seat_ids"`
	BuyerName  string  `json:"buyer_name"`
	BuyerEmail string  `json:"buyer_email"`
}

type orderItemResponse struct {
	Seat publicSeat `json:"seat"`
}

type createOrderResponse struct {
	Code         string              `json:"code"`
	ExpiresAt    time.Time           `json:"expires_at"`
	PayURL       string              `json:"pay_url"`
	TotalKopecks int64               `json:"total_kopecks"`
	Items        []orderItemResponse `json:"items"`
	BuyerName    string              `json:"buyer_name"`
	BuyerEmail   string              `json:"buyer_email"`
	TGDeepLink   string              `json:"tg_deep_link,omitempty"`
}

func (h *Handler) createOrder(w http.ResponseWriter, r *http.Request) {
	var req createOrderRequest
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
	if req.Slug == "" || len(req.SeatIDs) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_input", "slug and seat_ids required")
		return
	}
	if len(req.SeatIDs) > 20 {
		// Soft cap — keep one purchase from monopolising the room and
		// blowing up the email batch / Telegram chat message count.
		writeError(w, http.StatusBadRequest, "too_many_seats",
			"максимум 20 місць за одну покупку")
		return
	}

	show, err := h.st.LoadShowBySlug(r.Context(), req.Slug)
	if errors.Is(err, store.ErrShowNotFound) {
		writeError(w, http.StatusNotFound, "show_not_found", "")
		return
	}
	if err != nil {
		writeInternal(w, "load show", err)
		return
	}

	allSeats, err := h.st.Seats(r.Context(), show.ID)
	if err != nil {
		writeInternal(w, "list seats", err)
		return
	}
	byID := make(map[int64]store.Seat, len(allSeats))
	for _, s := range allSeats {
		byID[s.ID] = s
	}
	seenIDs := make(map[int64]struct{}, len(req.SeatIDs))
	targets := make([]store.Seat, 0, len(req.SeatIDs))
	for _, id := range req.SeatIDs {
		if _, dup := seenIDs[id]; dup {
			writeError(w, http.StatusBadRequest, "duplicate_seat",
				"seat included twice")
			return
		}
		seenIDs[id] = struct{}{}
		s, ok := byID[id]
		if !ok {
			writeError(w, http.StatusNotFound, "seat_not_found",
				"seat does not belong to this show")
			return
		}
		if !s.Sellable {
			writeError(w, http.StatusConflict, "seat_not_sellable", "")
			return
		}
		if s.PriceKopecks < h.priceMin {
			writeInternal(w, "seat below min price",
				fmt.Errorf("seat %d price %d < min %d", s.ID, s.PriceKopecks, h.priceMin))
			return
		}
		targets = append(targets, s)
	}

	code, err := h.coder.NewCode()
	if err != nil {
		writeInternal(w, "mint code", err)
		return
	}

	order, _, err := h.st.CreateOrder(r.Context(), targets, 0, 0, name, email, code, h.hold)
	switch {
	case errors.Is(err, store.ErrSeatTaken):
		writeError(w, http.StatusConflict, "seat_taken", "одне з місць щойно зайняли")
		return
	case errors.Is(err, store.ErrSeatNotSellable):
		writeError(w, http.StatusConflict, "seat_not_sellable", "")
		return
	case err != nil:
		writeInternal(w, "create order", err)
		return
	}

	payURL := jarPrefillURL(h.jarLink, order.TotalKopecks, order.Code)
	items := make([]orderItemResponse, 0, len(targets))
	for _, s := range targets {
		items = append(items, orderItemResponse{Seat: publicSeat{
			ID: s.ID, Row: s.Row, Col: s.Col,
			X: s.X, Y: s.Y, Label: s.Label, Category: s.Category,
			PriceKopecks: s.PriceKopecks, Sellable: true,
		}})
	}
	var tgLink string
	if h.botUsername != "" {
		tgLink = fmt.Sprintf("https://t.me/%s?start=res_%s", h.botUsername, order.Code)
	}
	slog.Info("public order created",
		"code", order.Code, "slug", show.Slug, "seats", len(targets),
		"total", order.TotalKopecks, "buyer", name, "email", email)

	writeJSON(w, http.StatusCreated, createOrderResponse{
		Code:         order.Code,
		ExpiresAt:    order.ExpiresAt,
		PayURL:       payURL,
		TotalKopecks: order.TotalKopecks,
		Items:        items,
		BuyerName:    name,
		BuyerEmail:   email,
		TGDeepLink:   tgLink,
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

