// Package public exposes the buyer-side JSON API: what's at /event/<slug>,
// what seats are still available, and the POST that creates a reservation
// for a web buyer (no Telegram involved).
//
// Endpoints land under /api/public/* — *no* auth required. Care has been
// taken not to surface admin-only data (tg_user_id, internal IDs unrelated
// to the seat map, full reservation history).
package public

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

	"github.com/OlexiyOdarchuk/monokasa/internal/realtime"
	"github.com/OlexiyOdarchuk/monokasa/internal/store"
	"github.com/OlexiyOdarchuk/monokasa/internal/token"
)

// LoginMailer sends the magic-link email when a buyer asks for one.
// Decoupled from the package's other concerns so tests can drop in a
// fake without involving SMTP.
type LoginMailer interface {
	SendLoginLink(ctx context.Context, to, link string) error
}

// Handler is the buyer-side API surface.
type Handler struct {
	st             *store.Store
	coder          *token.Coder
	jarLink        string // monobank jar URL; pay link is jarLink?a=…&t=…
	hold           time.Duration
	priceMin       int64  // minimum price kopecks; reservations below this are rejected
	botUsername    string // optional; when set, ReservationResponse carries a TG deep link
	hub            *realtime.Hub
	baseURL        string // public origin used when composing magic-link emails
	loginMailer    LoginMailer
	secureCookies  bool
}

type Config struct {
	Store         *store.Store
	Coder         *token.Coder
	JarLink       string
	Hold          time.Duration
	MinPrice      int64
	BotUsername   string        // optional Telegram bot @-handle (no leading "@")
	Hub           *realtime.Hub // optional; SSE seat updates skipped if nil
	BaseURL       string        // e.g. https://monokasa.app — needed for magic-link emails
	LoginMailer   LoginMailer   // optional; magic-link login disabled when nil
	SecureCookies bool          // true forces Secure cookie attribute in production
}

func NewHandler(c Config) *Handler {
	return &Handler{
		st:            c.Store,
		coder:         c.Coder,
		jarLink:       c.JarLink,
		hold:          c.Hold,
		priceMin:      c.MinPrice,
		botUsername:   c.BotUsername,
		hub:           c.Hub,
		baseURL:       strings.TrimRight(c.BaseURL, "/"),
		loginMailer:   c.LoginMailer,
		secureCookies: c.SecureCookies,
	}
}

// logAudit records a public-side action (buyer reserve / order create)
// in the same audit_log table the admin uses. ActorUserID stays 0 to
// mark this as not-an-admin; actorLabel carries whatever identifies the
// caller (typically buyer email). Audit failures are slog.Error'd and
// swallowed — losing one trail entry is less bad than failing the
// user-facing purchase.
func (h *Handler) logAudit(r *http.Request, action, target, actorLabel string, details map[string]any) {
	var raw string
	if len(details) > 0 {
		if b, err := json.Marshal(details); err == nil {
			raw = string(b)
		}
	}
	if err := h.st.LogAudit(r.Context(), store.AuditEntry{
		ActorUserID: 0,
		ActorEmail:  actorLabel,
		Action:      action,
		Target:      target,
		Details:     raw,
	}); err != nil {
		slog.Error("public audit write failed",
			"action", action, "target", target, "err", err)
		return
	}
	slog.Info("audit", "action", action, "target", target, "actor", actorLabel)
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/public/shows", h.listShows)
	mux.HandleFunc("GET /api/public/shows/{slug}", h.getShow)
	mux.HandleFunc("GET /api/public/shows/{slug}/events", h.events)
	mux.HandleFunc("POST /api/public/reservations", h.createReservation)
	mux.HandleFunc("POST /api/public/orders", h.createOrder)
	mux.HandleFunc("GET /api/public/reservations/{code}/status", h.reservationStatus)

	// Buyer "Мої квитки" magic-link auth.
	mux.HandleFunc("POST /api/public/login/request", h.loginRequest)
	mux.HandleFunc("GET /api/public/login/consume", h.loginConsume)
	mux.HandleFunc("POST /api/public/login/logout", h.loginLogout)
	mux.HandleFunc("GET /api/public/my", h.myWhoami)
	mux.HandleFunc("GET /api/public/my/tickets", h.myTickets)

	mux.HandleFunc("GET /api/public/organizer", h.getOrganizer)
}

// --- GET /api/public/organizer ---
//
// Single-row profile rendered by /about. Always returns 200 — empty
// fields are the "not yet configured" signal the SPA handles on its
// own.
type publicOrganizer struct {
	Name         string `json:"name"`
	Bio          string `json:"bio"`
	ContactEmail string `json:"contact_email"`
	Phone        string `json:"phone"`
	WebsiteURL   string `json:"website_url"`
	TelegramURL  string `json:"telegram_url"`
	InstagramURL string `json:"instagram_url"`
	FacebookURL  string `json:"facebook_url"`
	LogoURL      string `json:"logo_url"`
}

func (h *Handler) getOrganizer(w http.ResponseWriter, r *http.Request) {
	o, err := h.st.LoadOrganizer(r.Context())
	if err != nil {
		writeInternal(w, "load organizer", err)
		return
	}
	writeJSON(w, http.StatusOK, publicOrganizer{
		Name: o.Name, Bio: o.Bio, ContactEmail: o.ContactEmail, Phone: o.Phone,
		WebsiteURL: o.WebsiteURL, TelegramURL: o.TelegramURL,
		InstagramURL: o.InstagramURL, FacebookURL: o.FacebookURL,
		LogoURL: o.LogoURL,
	})
}

// --- GET /api/public/shows/{slug}/events ---
//
// Server-Sent Events stream of seat-status changes for one show. Buyers
// keep an EventSource open while viewing /event/<slug> so the seat map
// updates without polling. Disconnected clients are reaped via the
// request context — no manual goroutine bookkeeping here.
//
// When the hub is nil (tests / minimal builds) we return 503 so the
// client knows to fall back to its own polling cadence.

func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	if h.hub == nil {
		writeError(w, http.StatusServiceUnavailable, "realtime_disabled", "")
		return
	}
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

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Net/http always provides Flusher for HTTP/1.1 and h2, so this
		// path is only ever hit under a weird middleware that wraps the
		// response writer. Bail clearly.
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Disable buffering on intermediaries that honor it (nginx).
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, unsub := h.hub.Subscribe(show.ID)
	defer unsub()

	// Keep-alive comment line every 25s so proxies / browsers don't drop
	// an idle SSE connection. Comments (`:` prefix) are silently
	// ignored by EventSource.
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			payload, err := json.Marshal(ev)
			if err != nil {
				// Drop this frame; structurally impossible for our
				// own struct, but never panic on a stream.
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
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
	// Kind tells the lander whether this card links to a seat-map page
	// or a quantity picker. "seated" or "ga".
	Kind string `json:"kind"`
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
		kind := sh.Kind
		if kind == "" {
			kind = "seated"
		}
		out = append(out, publicShowSummary{
			Slug: sh.Slug, Title: sh.Title, Venue: sh.Venue,
			StartsAt:    sh.StartsAt,
			Description: sh.Description,
			PosterURL:   sh.PosterURL,
			SeatsFree:   st.Free,
			SeatsTotal:  st.Total,
			Kind:        kind,
		})
	}
	// Sort by start, soonest first.
	sort.Slice(out, func(i, j int) bool { return out[i].StartsAt.Before(out[j].StartsAt) })
	writeJSON(w, http.StatusOK, out)
}

// --- GET /api/public/shows/{slug} ---

type publicShow struct {
	Slug        string           `json:"slug"`
	Title       string           `json:"title"`
	Venue       string           `json:"venue"`
	StartsAt    time.Time        `json:"starts_at"`
	Description string           `json:"description"`
	PosterURL   string           `json:"poster_url"`
	Seats       []publicSeat     `json:"seats"`
	Categories  []publicCategory `json:"categories"`
	// Kind is "seated" or "ga". Frontend renders a quantity picker for
	// "ga" instead of the seat map.
	Kind string `json:"kind"`
	// GACapacity is the pool size at show creation. Used for the
	// "усього N квитків" label on the buyer page.
	GACapacity int `json:"ga_capacity,omitempty"`
	// GAPrice is the per-ticket price for GA shows. For seated shows
	// every seat carries its own price, so this stays 0.
	GAPrice int64 `json:"ga_price_kopecks,omitempty"`
	// GAFree is the live count of unsold/unheld GA tickets. Trivial to
	// compute on the frontend by counting Seats, but having it pre-counted
	// makes the quantity picker simpler and survives if Seats is omitted
	// in future for bandwidth.
	GAFree int `json:"ga_free,omitempty"`
}

type publicCategory struct {
	Name         string `json:"name"`
	Color        string `json:"color"`
	PriceKopecks int64  `json:"price_kopecks"`
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

	cats, err := h.st.ListSeatCategories(r.Context(), show.ID)
	if err != nil {
		// Non-fatal: render seats with default colour if categories
		// query trips. Log so the operator notices.
		slog.Warn("list categories failed", "showId", show.ID, "err", err)
	}
	kind := show.Kind
	if kind == "" {
		kind = "seated"
	}
	out := publicShow{
		Slug: show.Slug, Title: show.Title, Venue: show.Venue,
		StartsAt:    show.StartsAt,
		Description: show.Description,
		PosterURL:   show.PosterURL,
		Seats:       make([]publicSeat, 0, len(seats)),
		Categories:  make([]publicCategory, 0, len(cats)),
		Kind:        kind,
		GACapacity:  show.GACapacity,
	}
	for _, c := range cats {
		out.Categories = append(out.Categories, publicCategory{
			Name: c.Name, Color: c.Color, PriceKopecks: c.PriceKopecks,
		})
	}
	gaFree := 0
	for _, s := range seats {
		taken := statuses[s.ID] != store.SeatFree
		if kind == "ga" {
			// Price is uniform across the pool, but read it from the
			// first seat we see so a future "raise GA price" admin tweak
			// still flows through to the buyer.
			if out.GAPrice == 0 && s.PriceKopecks > 0 {
				out.GAPrice = s.PriceKopecks
			}
			if !taken && s.Sellable {
				gaFree++
			}
			// Don't ship per-seat rows to GA buyers — the picker only
			// cares about the free count, and shipping rows would leak
			// "seat 47 of 200 is taken" which is meaningless for GA.
			continue
		}
		out.Seats = append(out.Seats, publicSeat{
			ID: s.ID, Row: s.Row, Col: s.Col,
			X: s.X, Y: s.Y, Label: s.Label, Category: s.Category,
			PriceKopecks: s.PriceKopecks, Sellable: s.Sellable,
			Taken: taken,
		})
	}
	if kind == "ga" {
		out.GAFree = gaFree
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

	// Single-seat alias never collects an attendee name — every PDF
	// prints the buyer name. Multi-seat endpoint below threads the
	// optional attendee_names slice.
	order, _, err := h.st.CreateOrder(r.Context(), []store.Seat{target}, 0, 0, name, email, nil, code, h.hold)
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
	h.hub.Publish(show.ID, realtime.Event{
		Type: "seat_status", SeatID: target.ID, Status: realtime.SeatHeld,
	})
	h.logAudit(r, "order.create", fmt.Sprintf("order:%d", order.ID), email, map[string]any{
		"code": order.Code, "slug": show.Slug, "seats": 1,
		"buyer_name": name, "total_kopecks": target.PriceKopecks,
		"source": "web",
	})
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
	// AttendeeNames is optional. If present, must align 1:1 with SeatIDs
	// — each entry is the name printed on that ticket. Empty strings
	// inside fall back to BuyerName at render time. Omit the field
	// entirely (or pass an empty slice) to use BuyerName for every ticket.
	AttendeeNames []string `json:"attendee_names,omitempty"`
	// Quantity is the GA-mode shortcut: buyer doesn't pick specific
	// seats, server allocates Quantity free virtual seats from the pool.
	// Mutually exclusive with SeatIDs — providing both is an error.
	Quantity int `json:"quantity,omitempty"`
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
	if req.Slug == "" {
		writeError(w, http.StatusBadRequest, "invalid_input", "slug required")
		return
	}
	if len(req.SeatIDs) > 0 && req.Quantity > 0 {
		writeError(w, http.StatusBadRequest, "invalid_input",
			"pass either seat_ids or quantity, not both")
		return
	}
	if len(req.SeatIDs) == 0 && req.Quantity <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_input",
			"seat_ids or quantity required")
		return
	}
	// Soft cap on both paths — keep one purchase from monopolising the
	// room and blowing up the email batch / Telegram chat message count.
	if len(req.SeatIDs) > 20 || req.Quantity > 20 {
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

	// GA quantity-mode and seated seat-ids each get validated separately,
	// then converge on a `targets []store.Seat` slice the rest of the
	// handler treats uniformly. GA path also disallows attendee_names —
	// GA tickets are interchangeable and the PDF won't render row/col,
	// so per-ticket naming would be invisible.
	var targets []store.Seat
	var attendees []string

	if req.Quantity > 0 {
		if !show.IsGA() {
			writeError(w, http.StatusBadRequest, "invalid_input",
				"quantity is only valid for GA shows")
			return
		}
		if len(req.AttendeeNames) > 0 {
			writeError(w, http.StatusBadRequest, "invalid_input",
				"attendee_names not supported in GA mode")
			return
		}
		picked, err := h.st.AllocateFreeSeats(r.Context(), show.ID, req.Quantity)
		if errors.Is(err, store.ErrSeatTaken) {
			writeError(w, http.StatusConflict, "not_enough_seats",
				"замало вільних квитків")
			return
		}
		if err != nil {
			writeInternal(w, "allocate ga seats", err)
			return
		}
		for _, s := range picked {
			if s.PriceKopecks < h.priceMin {
				writeInternal(w, "seat below min price",
					fmt.Errorf("seat %d price %d < min %d", s.ID, s.PriceKopecks, h.priceMin))
				return
			}
		}
		targets = picked
	} else {
		if show.IsGA() {
			writeError(w, http.StatusBadRequest, "invalid_input",
				"GA shows require quantity, not seat_ids")
			return
		}
		// AttendeeNames are optional. Accept either omitted/empty (every
		// ticket prints BuyerName) or a slice matching SeatIDs 1:1.
		if len(req.AttendeeNames) > 0 {
			if len(req.AttendeeNames) != len(req.SeatIDs) {
				writeError(w, http.StatusBadRequest, "invalid_input",
					"attendee_names length must match seat_ids")
				return
			}
			attendees = make([]string, len(req.AttendeeNames))
			for i, n := range req.AttendeeNames {
				n = strings.TrimSpace(n)
				if n == "" {
					attendees[i] = ""
					continue
				}
				normalized, err := normalizeName(n)
				if err != nil {
					writeError(w, http.StatusBadRequest, "invalid_attendee_name",
						fmt.Sprintf("seat %d: %s", i+1, err.Error()))
					return
				}
				attendees[i] = normalized
			}
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
		targets = make([]store.Seat, 0, len(req.SeatIDs))
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
	}

	code, err := h.coder.NewCode()
	if err != nil {
		writeInternal(w, "mint code", err)
		return
	}

	order, _, err := h.st.CreateOrder(r.Context(), targets, 0, 0, name, email, attendees, code, h.hold)
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
		h.hub.Publish(show.ID, realtime.Event{
			Type: "seat_status", SeatID: s.ID, Status: realtime.SeatHeld,
		})
	}
	var tgLink string
	if h.botUsername != "" {
		tgLink = fmt.Sprintf("https://t.me/%s?start=res_%s", h.botUsername, order.Code)
	}
	h.logAudit(r, "order.create", fmt.Sprintf("order:%d", order.ID), email, map[string]any{
		"code": order.Code, "slug": show.Slug, "seats": len(targets),
		"buyer_name": name, "total_kopecks": order.TotalKopecks,
		"source": "web",
	})
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

// --- buyer auth (magic link) ---

// buyerSessionCookie is the HttpOnly cookie holding the buyer's
// long-lived session token. Separate from monokasa_admin so an admin
// using the same browser doesn't collide.
const buyerSessionCookie = "monokasa_buyer"

type loginRequestBody struct {
	Email string `json:"email"`
}

// loginRequest mints a fresh login token, emails the magic link, and
// returns a generic ok response. Doesn't leak whether the email
// matched an existing buyer — anyone can request a link, and only
// the inbox owner can use it.
//
// Dev-friendly: missing BASE_URL falls back to the request's own
// scheme+host (covers local `make run` testing). Missing SMTP logs
// the link to slog at WARN level instead of returning an error —
// useful for trying the flow locally before signing up to Resend.
// Both fallbacks are visibly noisy so an operator notices in prod.
func (h *Handler) loginRequest(w http.ResponseWriter, r *http.Request) {
	var req loginRequestBody
	if !decodeJSON(w, r, &req) {
		return
	}
	email, err := normalizeEmail(req.Email)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_email", err.Error())
		return
	}
	token, err := randomToken()
	if err != nil {
		writeInternal(w, "mint login token", err)
		return
	}
	if err := h.st.CreateBuyerLoginToken(r.Context(), token, email); err != nil {
		writeInternal(w, "create login token", err)
		return
	}
	// Magic link points at the consume endpoint directly, not /my?token=.
	// That way the browser's own navigation processes the 303 Set-Cookie
	// + Location:/my naturally — `fetch(..., {redirect:'manual'})` drops
	// the cookie on the floor in some browsers, which used to leave the
	// buyer staring at the login form even after a "successful" click.
	link := fmt.Sprintf("%s/api/public/login/consume?token=%s",
		h.publicOrigin(r), url.QueryEscape(token))
	if h.loginMailer == nil {
		// No SMTP configured. Log the link so the developer can copy
		// it from console, but advertise that this is not safe for
		// production (anyone reading logs can hijack the session).
		slog.Warn("SMTP not configured — magic link printed in logs (NOT SAFE FOR PRODUCTION)",
			"email", email, "link", link)
		writeJSON(w, http.StatusOK, map[string]string{"status": "logged"})
		return
	}
	// Send asynchronously — SMTP can be slow, no reason to make the
	// buyer wait for the round-trip. Errors stay in the slog.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := h.loginMailer.SendLoginLink(ctx, email, link); err != nil {
			slog.Error("send login link", "email", email, "err", err)
			return
		}
		slog.Info("login link sent", "email", email)
	}()
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

// publicOrigin returns BASE_URL when configured, otherwise reconstructs
// scheme+host from the request. Both forms drop the trailing slash so
// callers can append paths cleanly.
func (h *Handler) publicOrigin(r *http.Request) string {
	if h.baseURL != "" {
		return h.baseURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	return scheme + "://" + r.Host
}

// loginConsume validates the token from the magic link, mints a long-
// lived cookie, and 303s the browser to /my. Errors also 303 back to
// /my (with ?error=…) so the SPA renders a friendly message instead
// of the buyer seeing raw JSON in the address bar.
//
// Hit directly by the magic link (not by the SPA via fetch). That way
// the browser handles Set-Cookie + Location naturally — earlier fetch-
// based plumbing dropped the cookie on some browsers and silently
// left the buyer on the login form.
func (h *Handler) loginConsume(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Redirect(w, r, "/my?error=missing_token", http.StatusSeeOther)
		return
	}
	email, err := h.st.ConsumeBuyerLoginToken(r.Context(), token)
	switch {
	case errors.Is(err, store.ErrCodeNotFound):
		http.Redirect(w, r, "/my?error=invalid_token", http.StatusSeeOther)
		return
	case errors.Is(err, store.ErrAlreadyClosed):
		http.Redirect(w, r, "/my?error=expired_token", http.StatusSeeOther)
		return
	case err != nil:
		slog.Error("consume login token", "err", err)
		http.Redirect(w, r, "/my?error=internal", http.StatusSeeOther)
		return
	}
	sessionToken, err := randomToken()
	if err != nil {
		slog.Error("mint session token", "err", err)
		http.Redirect(w, r, "/my?error=internal", http.StatusSeeOther)
		return
	}
	if err := h.st.CreateBuyerSession(r.Context(), sessionToken, email); err != nil {
		slog.Error("create buyer session", "err", err)
		http.Redirect(w, r, "/my?error=internal", http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     buyerSessionCookie,
		Value:    sessionToken,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cookieSecure(r),
		MaxAge:   int(store.BuyerSessionTTL.Seconds()),
	})
	http.Redirect(w, r, "/my", http.StatusSeeOther)
}

// loginLogout clears the cookie + session row.
func (h *Handler) loginLogout(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie(buyerSessionCookie); err == nil {
		_ = h.st.DeleteBuyerSession(r.Context(), ck.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: buyerSessionCookie, Value: "", Path: "/",
		HttpOnly: true, MaxAge: -1, Secure: h.cookieSecure(r),
	})
	w.WriteHeader(http.StatusNoContent)
}

// myWhoami returns the email tied to the current cookie, or 401.
// Frontend uses this to decide login form vs ticket list on /my.
func (h *Handler) myWhoami(w http.ResponseWriter, r *http.Request) {
	email, ok := h.buyerFromRequest(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "no_session", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"email": email})
}

type buyerTicketResponse struct {
	OrderID         int64                 `json:"order_id"`
	OrderCode       string                `json:"order_code"`
	OrderStatus     string                `json:"order_status"`
	TotalKopecks    int64                 `json:"total_kopecks"`
	CreatedAt       time.Time             `json:"created_at"`
	Show            buyerTicketShow       `json:"show"`
	Items           []buyerTicketItem     `json:"items"`
}

type buyerTicketShow struct {
	Slug      string    `json:"slug"`
	Title     string    `json:"title"`
	Venue     string    `json:"venue"`
	StartsAt  time.Time `json:"starts_at"`
	PosterURL string    `json:"poster_url,omitempty"`
}

type buyerTicketItem struct {
	ReservationID   int64      `json:"reservation_id"`
	Row             int        `json:"row"`
	Col             int        `json:"col"`
	Label           string     `json:"label,omitempty"`
	AttendeeName    string     `json:"attendee_name,omitempty"`
	PriceKopecks    int64      `json:"price_kopecks"`
	CancelledAt     *time.Time `json:"cancelled_at,omitempty"`
	RefundedAt      *time.Time `json:"refunded_at,omitempty"`
	QRPayload       string     `json:"qr_payload,omitempty"`
	UsedAt          *time.Time `json:"used_at,omitempty"`
}

// myTickets returns every order under the current buyer's email,
// grouped by order, with QR payloads on the items so the frontend can
// render scannable QR codes inline.
func (h *Handler) myTickets(w http.ResponseWriter, r *http.Request) {
	email, ok := h.buyerFromRequest(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "no_session", "")
		return
	}
	rows, err := h.st.BuyerTicketsByEmail(r.Context(), email)
	if err != nil {
		writeInternal(w, "buyer tickets", err)
		return
	}
	// Group by Order.ID preserving the row order (which is order
	// created_at DESC, reservation id ASC from the SQL).
	byOrder := make(map[int64]int)
	out := make([]buyerTicketResponse, 0)
	for _, row := range rows {
		idx, ok := byOrder[row.Order.ID]
		if !ok {
			status := "held"
			switch {
			case row.Order.CancelledAt != nil:
				status = "cancelled"
			case row.Order.ConfirmedAt != nil:
				status = "paid"
			case row.Order.ExpiresAt.Before(time.Now()):
				status = "expired"
			}
			out = append(out, buyerTicketResponse{
				OrderID: row.Order.ID, OrderCode: row.Order.Code,
				OrderStatus: status, TotalKopecks: row.Order.TotalKopecks,
				CreatedAt: row.Order.CreatedAt,
				Show: buyerTicketShow{
					Slug: row.Show.Slug, Title: row.Show.Title, Venue: row.Show.Venue,
					StartsAt: row.Show.StartsAt, PosterURL: row.Show.PosterURL,
				},
			})
			idx = len(out) - 1
			byOrder[row.Order.ID] = idx
		}
		out[idx].Items = append(out[idx].Items, buyerTicketItem{
			ReservationID: row.Reservation.ID,
			Row:           row.Seat.Row, Col: row.Seat.Col, Label: row.Seat.Label,
			AttendeeName: row.Reservation.AttendeeName,
			PriceKopecks: row.Seat.PriceKopecks,
			CancelledAt:  row.Reservation.CancelledAt,
			RefundedAt:   row.Reservation.RefundedAt,
			QRPayload:    row.QRPayload,
			UsedAt:       row.UsedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// buyerFromRequest reads the session cookie + resolves to an email.
func (h *Handler) buyerFromRequest(r *http.Request) (string, bool) {
	ck, err := r.Cookie(buyerSessionCookie)
	if err != nil || ck.Value == "" {
		return "", false
	}
	email, err := h.st.FindBuyerSession(r.Context(), ck.Value)
	if err != nil || email == "" {
		return "", false
	}
	return email, true
}

// cookieSecure mirrors auth.Handler.setCookie's logic: honour the
// explicit config flag, otherwise auto-detect HTTPS via TLS or the
// X-Forwarded-Proto header (cloudflared, nginx).
func (h *Handler) cookieSecure(r *http.Request) bool {
	if h.secureCookies {
		return true
	}
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// randomToken returns a 256-bit hex string suitable for a cookie or
// magic-link token. Crypto/rand-backed; no namespace collisions to
// worry about given the 64-char keyspace.
func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// --- helpers ---

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
