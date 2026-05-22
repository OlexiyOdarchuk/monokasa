// Package admin exposes the JSON API the Svelte admin UI talks to.
// Everything here lives behind auth.RequireAuth — main.go wraps the
// admin mux before mounting it at /api/admin/.
//
// Conventions:
//   - All bodies are application/json with snake_case fields.
//   - Times are RFC3339 strings.
//   - Errors are {"error": "<machine code>", "detail": "<human>"} with
//     an appropriate 4xx/5xx status.
//   - Specific HTTP methods are declared on each route via Go 1.22's
//     mux pattern so a wrong verb yields 405 from the mux itself.
package admin

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/OlexiyOdarchuk/monokasa/internal/auth"
	"github.com/OlexiyOdarchuk/monokasa/internal/realtime"
	"github.com/OlexiyOdarchuk/monokasa/internal/store"
)

// CancelNotifier is called after admin.cancelReservation succeeds. The
// handler doesn't care where the notification goes (Telegram, email,
// both) — main.go composes a function that does the right thing for the
// reservation's contact details. Errors get logged but don't fail the
// admin response: the DB cancel is already durable.
type CancelNotifier func(ctx context.Context, res store.Reservation, seat store.Seat)

// Handler aggregates the admin endpoints. Pass *store.Store directly — the
// admin package is internal infrastructure, no reason to plumb yet another
// interface tier.
type Handler struct {
	st       *store.Store
	onCancel CancelNotifier // optional; nil is a no-op
	hub      *realtime.Hub  // optional; nil → no SSE publish on cancel
}

func NewHandler(st *store.Store) *Handler {
	return &Handler{st: st}
}

// SetCancelNotifier wires the post-cancel notification hook. Optional;
// when nil the cancel endpoint just updates the DB.
func (h *Handler) SetCancelNotifier(fn CancelNotifier) { h.onCancel = fn }

// SetHub wires the realtime pub/sub so cancel actions broadcast a
// seat-status event to live SSE subscribers.
func (h *Handler) SetHub(hub *realtime.Hub) { h.hub = hub }

// Register wires every endpoint onto the given mux. The mux is meant to
// be wrapped by auth.RequireAuth at the call site, not by this package.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin/me", h.me)

	mux.HandleFunc("GET /api/admin/shows", h.listShows)
	mux.HandleFunc("POST /api/admin/shows", h.createShow)
	mux.HandleFunc("GET /api/admin/shows/{id}", h.getShow)
	mux.HandleFunc("PATCH /api/admin/shows/{id}", h.updateShow)
	mux.HandleFunc("POST /api/admin/shows/{id}/archive", h.archiveShow)

	mux.HandleFunc("GET /api/admin/shows/{id}/seats", h.listSeats)
	mux.HandleFunc("POST /api/admin/shows/{id}/seats", h.addSeat)
	mux.HandleFunc("PATCH /api/admin/seats", h.batchUpdateSeats)
	mux.HandleFunc("DELETE /api/admin/seats/{id}", h.removeSeat)

	mux.HandleFunc("GET /api/admin/shows/{id}/guests", h.listGuests)
	mux.HandleFunc("GET /api/admin/shows/{id}/guests.csv", h.exportGuestsCSV)
	mux.HandleFunc("POST /api/admin/reservations/{id}/cancel", h.cancelReservation)
	mux.HandleFunc("POST /api/admin/reservations/{id}/refund", h.markRefunded)

	mux.HandleFunc("GET /api/admin/audit", h.listAudit)
}

// --- /me ---

type meResponse struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFromContext(r.Context())
	if !ok {
		// Should be unreachable behind RequireAuth, but defend anyway.
		writeError(w, http.StatusUnauthorized, "no_session", "session missing")
		return
	}
	writeJSON(w, http.StatusOK, meResponse{ID: u.ID, Email: u.Email, Name: u.Name})
}

// audit writes a row to audit_log without bothering the caller about
// the outcome. Audit failures get logged and swallowed — losing a
// trail line is less bad than failing the user-facing action.
//
// Failure paths are logged at Error (not Warn) so operators see them
// in default-level logs; the missing-user branch was previously a
// silent return which made "empty audit list" impossible to diagnose.
func (h *Handler) audit(r *http.Request, action, target string, details map[string]any) {
	u, ok := auth.UserFromContext(r.Context())
	if !ok {
		slog.Error("audit: no user in context — RequireAuth middleware misconfigured?",
			"action", action, "target", target)
		return
	}
	var raw string
	if len(details) > 0 {
		if b, err := json.Marshal(details); err == nil {
			raw = string(b)
		}
	}
	if err := h.st.LogAudit(r.Context(), store.AuditEntry{
		ActorUserID: u.ID,
		ActorEmail:  u.Email,
		Action:      action,
		Target:      target,
		Details:     raw,
	}); err != nil {
		slog.Error("audit log write failed",
			"action", action, "target", target, "actor", u.Email, "err", err)
		return
	}
	slog.Info("audit", "action", action, "target", target, "actor", u.Email)
}

// --- shows ---

type showResponse struct {
	ID          int64      `json:"id"`
	Slug        string     `json:"slug"`
	Title       string     `json:"title"`
	Venue       string     `json:"venue"`
	StartsAt    time.Time  `json:"starts_at"`
	Description string     `json:"description"`
	PosterURL   string     `json:"poster_url"`
	CreatedAt   time.Time  `json:"created_at"`
	ArchivedAt  *time.Time `json:"archived_at,omitempty"`
	Stats       *statsBody `json:"stats,omitempty"`
}

type statsBody struct {
	Total          int   `json:"total"`
	Sold           int   `json:"sold"`
	Held           int   `json:"held"`
	Free           int   `json:"free"`
	RevenueKopecks int64 `json:"revenue_kopecks"`
}

func toShowResponse(s store.Show, stats *statsBody) showResponse {
	return showResponse{
		ID: s.ID, Slug: s.Slug, Title: s.Title, Venue: s.Venue,
		StartsAt: s.StartsAt, Description: s.Description, PosterURL: s.PosterURL,
		CreatedAt: s.CreatedAt, ArchivedAt: s.ArchivedAt,
		Stats: stats,
	}
}

func (h *Handler) listShows(w http.ResponseWriter, r *http.Request) {
	shows, err := h.st.ListShows(r.Context())
	if err != nil {
		writeInternal(w, "list shows", err)
		return
	}
	out := make([]showResponse, len(shows))
	for i, s := range shows {
		out[i] = toShowResponse(s, nil)
	}
	writeJSON(w, http.StatusOK, out)
}

type createShowRequest struct {
	Title        string    `json:"title"`
	Venue        string    `json:"venue"`
	StartsAt     time.Time `json:"starts_at"`
	Rows         int       `json:"rows"`
	Cols         int       `json:"cols"`
	PriceKopecks int64     `json:"price_kopecks"`
}

func (h *Handler) createShow(w http.ResponseWriter, r *http.Request) {
	var req createShowRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Title == "" || req.Rows <= 0 || req.Cols <= 0 || req.PriceKopecks <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_input",
			"title, rows>0, cols>0 and price_kopecks>0 are required")
		return
	}
	id, err := h.st.CreateShow(r.Context(), store.Show{
		Title: req.Title, Venue: req.Venue, StartsAt: req.StartsAt,
	}, req.Rows, req.Cols, req.PriceKopecks)
	if err != nil {
		writeInternal(w, "create show", err)
		return
	}
	sh, err := h.st.LoadShow(r.Context(), id)
	if err != nil {
		writeInternal(w, "load created show", err)
		return
	}
	h.audit(r, "show.create", fmt.Sprintf("show:%d", id), map[string]any{
		"title": sh.Title, "venue": sh.Venue, "starts_at": sh.StartsAt,
		"rows": req.Rows, "cols": req.Cols, "price_kopecks": req.PriceKopecks,
	})
	writeJSON(w, http.StatusCreated, toShowResponse(sh, nil))
}

func (h *Handler) getShow(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	sh, err := h.st.LoadShow(r.Context(), id)
	if errors.Is(err, store.ErrShowNotFound) {
		writeError(w, http.StatusNotFound, "show_not_found", "")
		return
	}
	if err != nil {
		writeInternal(w, "load show", err)
		return
	}
	st, err := h.st.Stats(r.Context(), id)
	if err != nil {
		writeInternal(w, "stats", err)
		return
	}
	writeJSON(w, http.StatusOK, toShowResponse(sh, &statsBody{
		Total: st.Total, Sold: st.Sold, Held: st.Held, Free: st.Free,
		RevenueKopecks: st.RevenueKopecks,
	}))
}

type updateShowRequest struct {
	Title       *string    `json:"title"`
	Venue       *string    `json:"venue"`
	StartsAt    *time.Time `json:"starts_at"`
	Description *string    `json:"description"`
	PosterURL   *string    `json:"poster_url"`
}

func (h *Handler) updateShow(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	var req updateShowRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	current, err := h.st.LoadShow(r.Context(), id)
	if errors.Is(err, store.ErrShowNotFound) {
		writeError(w, http.StatusNotFound, "show_not_found", "")
		return
	}
	if err != nil {
		writeInternal(w, "load show", err)
		return
	}
	// Build patch on top of current values so the client can PATCH
	// individual fields without resending the whole row.
	merged := current
	if req.Title != nil {
		merged.Title = *req.Title
	}
	if req.Venue != nil {
		merged.Venue = *req.Venue
	}
	if req.StartsAt != nil {
		merged.StartsAt = *req.StartsAt
	}
	if req.Description != nil {
		merged.Description = *req.Description
	}
	if req.PosterURL != nil {
		merged.PosterURL = *req.PosterURL
	}
	if err := h.st.UpdateShow(r.Context(), merged); err != nil {
		writeInternal(w, "update show", err)
		return
	}
	// Track which fields actually changed so the audit row is useful
	// without a separate "before/after" diff in the UI.
	changed := map[string]any{}
	if req.Title != nil {
		changed["title"] = merged.Title
	}
	if req.Venue != nil {
		changed["venue"] = merged.Venue
	}
	if req.StartsAt != nil {
		changed["starts_at"] = merged.StartsAt
	}
	if req.Description != nil {
		changed["description"] = merged.Description
	}
	if req.PosterURL != nil {
		changed["poster_url"] = merged.PosterURL
	}
	h.audit(r, "show.update", fmt.Sprintf("show:%d", id), changed)
	writeJSON(w, http.StatusOK, toShowResponse(merged, nil))
}

func (h *Handler) archiveShow(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	err := h.st.ArchiveShow(r.Context(), id)
	if errors.Is(err, store.ErrShowNotFound) {
		writeError(w, http.StatusNotFound, "show_not_found",
			"show does not exist or is already archived")
		return
	}
	if err != nil {
		writeInternal(w, "archive show", err)
		return
	}
	h.audit(r, "show.archive", fmt.Sprintf("show:%d", id), nil)
	w.WriteHeader(http.StatusNoContent)
}

// --- seats ---

type seatResponse struct {
	ID           int64   `json:"id"`
	ShowID       int64   `json:"show_id"`
	Row          int     `json:"row"`
	Col          int     `json:"col"`
	X            float64 `json:"x"`
	Y            float64 `json:"y"`
	Label        string  `json:"label"`
	Category     string  `json:"category"`
	PriceKopecks int64   `json:"price_kopecks"`
	Sellable     bool    `json:"sellable"`
}

func toSeatResponse(s store.Seat) seatResponse {
	return seatResponse{
		ID: s.ID, ShowID: s.ShowID, Row: s.Row, Col: s.Col,
		X: s.X, Y: s.Y, Label: s.Label, Category: s.Category,
		PriceKopecks: s.PriceKopecks, Sellable: s.Sellable,
	}
}

func (h *Handler) listSeats(w http.ResponseWriter, r *http.Request) {
	showID, ok := parseID(w, r)
	if !ok {
		return
	}
	seats, err := h.st.Seats(r.Context(), showID)
	if err != nil {
		writeInternal(w, "list seats", err)
		return
	}
	out := make([]seatResponse, len(seats))
	for i, s := range seats {
		out[i] = toSeatResponse(s)
	}
	writeJSON(w, http.StatusOK, out)
}

type addSeatRequest struct {
	Row          int     `json:"row"`
	Col          int     `json:"col"`
	X            float64 `json:"x"`
	Y            float64 `json:"y"`
	Label        string  `json:"label"`
	Category     string  `json:"category"`
	PriceKopecks int64   `json:"price_kopecks"`
	Sellable     bool    `json:"sellable"`
}

func (h *Handler) addSeat(w http.ResponseWriter, r *http.Request) {
	showID, ok := parseID(w, r)
	if !ok {
		return
	}
	var req addSeatRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Row <= 0 || req.Col <= 0 || req.PriceKopecks < 0 {
		writeError(w, http.StatusBadRequest, "invalid_input",
			"row>0, col>0 and price_kopecks>=0 required")
		return
	}
	seat, err := h.st.AddSeat(r.Context(), store.NewSeat{
		ShowID: showID, Row: req.Row, Col: req.Col, X: req.X, Y: req.Y,
		Label: req.Label, Category: req.Category,
		PriceKopecks: req.PriceKopecks, Sellable: req.Sellable,
	})
	if errors.Is(err, store.ErrSeatExists) {
		writeError(w, http.StatusConflict, "seat_exists",
			"row/col already taken in this show")
		return
	}
	if err != nil {
		writeInternal(w, "add seat", err)
		return
	}
	h.audit(r, "seat.add", fmt.Sprintf("seat:%d", seat.ID), map[string]any{
		"show_id": showID, "row": seat.Row, "col": seat.Col,
		"price_kopecks": seat.PriceKopecks,
	})
	writeJSON(w, http.StatusCreated, toSeatResponse(seat))
}

type seatPatchRequest struct {
	ID           int64    `json:"id"`
	X            *float64 `json:"x"`
	Y            *float64 `json:"y"`
	Label        *string  `json:"label"`
	Category     *string  `json:"category"`
	PriceKopecks *int64   `json:"price_kopecks"`
	Sellable     *bool    `json:"sellable"`
}

func (h *Handler) batchUpdateSeats(w http.ResponseWriter, r *http.Request) {
	var req []seatPatchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	patches := make([]store.SeatPatch, len(req))
	for i, p := range req {
		if p.ID == 0 {
			writeError(w, http.StatusBadRequest, "invalid_input",
				"each patch needs a non-zero id")
			return
		}
		patches[i] = store.SeatPatch{
			ID: p.ID, X: p.X, Y: p.Y,
			Label: p.Label, Category: p.Category,
			PriceKopecks: p.PriceKopecks, Sellable: p.Sellable,
		}
	}
	if err := h.st.UpdateSeats(r.Context(), patches); err != nil {
		if errors.Is(err, store.ErrSeatNotFound) {
			writeError(w, http.StatusNotFound, "seat_not_found",
				"one of the patched seats no longer exists")
			return
		}
		writeInternal(w, "batch update seats", err)
		return
	}
	ids := make([]int64, len(patches))
	for i, p := range patches {
		ids[i] = p.ID
	}
	h.audit(r, "seat.batch_update", "seats", map[string]any{
		"count": len(patches), "ids": ids,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) removeSeat(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	err := h.st.RemoveSeat(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrSeatNotFound):
		writeError(w, http.StatusNotFound, "seat_not_found", "")
		return
	case errors.Is(err, store.ErrSeatHasReservations):
		writeError(w, http.StatusConflict, "seat_has_reservations",
			"seat has reservation history and cannot be removed")
		return
	case err != nil:
		writeInternal(w, "remove seat", err)
		return
	}
	h.audit(r, "seat.remove", fmt.Sprintf("seat:%d", id), nil)
	w.WriteHeader(http.StatusNoContent)
}

// --- guests / reservations ---

type guestResponse struct {
	Reservation reservationBody `json:"reservation"`
	Seat        seatBriefBody   `json:"seat"`
}

type reservationBody struct {
	ID          int64      `json:"id"`
	Code        string     `json:"code"`
	BuyerName   string     `json:"buyer_name"`
	TGUserID    int64      `json:"tg_user_id"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
	CancelledAt *time.Time `json:"cancelled_at,omitempty"`
	RefundedAt  *time.Time `json:"refunded_at,omitempty"`
	Status      string     `json:"status"` // paid | held | expired | cancelled
}

type seatBriefBody struct {
	ID           int64  `json:"id"`
	Row          int    `json:"row"`
	Col          int    `json:"col"`
	Label        string `json:"label"`
	Category     string `json:"category"`
	PriceKopecks int64  `json:"price_kopecks"`
}

func reservationStatus(r store.Reservation, now time.Time) string {
	switch {
	case r.CancelledAt != nil:
		return "cancelled"
	case r.ConfirmedAt != nil:
		return "paid"
	case r.ExpiresAt.Before(now):
		return "expired"
	default:
		return "held"
	}
}

func toGuestResponse(item store.MyItem, now time.Time) guestResponse {
	return guestResponse{
		Reservation: reservationBody{
			ID: item.Reservation.ID, Code: item.Reservation.Code,
			BuyerName: item.Reservation.BuyerName, TGUserID: item.Reservation.TGUserID,
			CreatedAt: item.Reservation.CreatedAt, ExpiresAt: item.Reservation.ExpiresAt,
			ConfirmedAt: item.Reservation.ConfirmedAt, CancelledAt: item.Reservation.CancelledAt,
			RefundedAt: item.OrderRefundedAt,
			Status:     reservationStatus(item.Reservation, now),
		},
		Seat: seatBriefBody{
			ID: item.Seat.ID, Row: item.Seat.Row, Col: item.Seat.Col,
			Label: item.Seat.Label, Category: item.Seat.Category,
			PriceKopecks: item.Seat.PriceKopecks,
		},
	}
}

func (h *Handler) listGuests(w http.ResponseWriter, r *http.Request) {
	showID, ok := parseID(w, r)
	if !ok {
		return
	}
	items, err := h.st.ListReservations(r.Context(), showID)
	if err != nil {
		writeInternal(w, "list reservations", err)
		return
	}
	now := time.Now()
	out := make([]guestResponse, len(items))
	for i, it := range items {
		out[i] = toGuestResponse(it, now)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) exportGuestsCSV(w http.ResponseWriter, r *http.Request) {
	showID, ok := parseID(w, r)
	if !ok {
		return
	}
	items, err := h.st.ListReservations(r.Context(), showID)
	if err != nil {
		writeInternal(w, "list reservations for csv", err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="guests-show-%d.csv"`, showID))
	// UTF-8 BOM so Excel opens the file in the right encoding without manual fiddling.
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF})

	cw := csv.NewWriter(w)
	cw.Comma = ';' // most UA Excel installs default to ; as separator
	_ = cw.Write([]string{
		"code", "buyer_name", "row", "col", "label", "category",
		"price_uah", "status", "created_at", "confirmed_at", "cancelled_at",
	})
	now := time.Now()
	for _, it := range items {
		_ = cw.Write([]string{
			it.Reservation.Code,
			it.Reservation.BuyerName,
			strconv.Itoa(it.Seat.Row),
			strconv.Itoa(it.Seat.Col),
			it.Seat.Label,
			it.Seat.Category,
			formatPrice(it.Seat.PriceKopecks),
			reservationStatus(it.Reservation, now),
			it.Reservation.CreatedAt.Format(time.RFC3339),
			formatNullTime(it.Reservation.ConfirmedAt),
			formatNullTime(it.Reservation.CancelledAt),
		})
	}
	cw.Flush()
}

func (h *Handler) cancelReservation(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	res, seat, freed, err := h.st.AdminCancelReservation(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrCodeNotFound):
		writeError(w, http.StatusNotFound, "reservation_not_found", "")
		return
	case errors.Is(err, store.ErrAlreadyClosed):
		writeError(w, http.StatusConflict, "already_cancelled", "")
		return
	case err != nil:
		writeInternal(w, "admin cancel", err)
		return
	}
	// Best-effort fan-out to TG/email. Run async so admin doesn't wait
	// for SMTP — failures get logged inside the notifier.
	if h.onCancel != nil {
		go h.onCancel(context.Background(), res, seat)
	}
	// Broadcast every freed seat — multi-seat cancel cascades, so any
	// live SSE subscriber should see the whole basket flip back to free.
	for _, s := range freed {
		h.hub.Publish(s.ShowID, realtime.Event{
			Type: "seat_status", SeatID: s.ID, Status: realtime.SeatFree,
		})
	}
	h.audit(r, "reservation.cancel", fmt.Sprintf("reservation:%d", id), map[string]any{
		"buyer_name": res.BuyerName, "buyer_email": res.BuyerEmail,
		"freed_seats": len(freed),
	})
	writeJSON(w, http.StatusOK, toGuestResponse(store.MyItem{Reservation: res, Seat: seat}, time.Now()))
}

// markRefunded stamps orders.refunded_at — pure bookkeeping. Seat
// status is untouched (use cancel to free a seat). Refusable when the
// order isn't confirmed, or already marked.
func (h *Handler) markRefunded(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	order, err := h.st.MarkOrderRefunded(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrCodeNotFound):
		writeError(w, http.StatusNotFound, "reservation_not_found", "")
		return
	case errors.Is(err, store.ErrNotPaid):
		writeError(w, http.StatusConflict, "not_paid",
			"order is not confirmed — refund mark only applies after payment")
		return
	case errors.Is(err, store.ErrAlreadyClosed):
		writeError(w, http.StatusConflict, "already_refunded", "")
		return
	case err != nil:
		writeInternal(w, "mark refunded", err)
		return
	}
	h.audit(r, "order.refund_marked", fmt.Sprintf("order:%d", order.ID), map[string]any{
		"code": order.Code, "total_kopecks": order.TotalKopecks,
		"buyer_name": order.BuyerName, "buyer_email": order.BuyerEmail,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"order_id":     order.ID,
		"code":         order.Code,
		"refunded_at":  order.RefundedAt,
	})
}

// --- audit ---

type auditResponse struct {
	ID          int64           `json:"id"`
	ActorUserID int64           `json:"actor_user_id"`
	ActorEmail  string          `json:"actor_email"`
	Action      string          `json:"action"`
	Target      string          `json:"target"`
	Details     json.RawMessage `json:"details,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

func (h *Handler) listAudit(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	entries, err := h.st.ListAuditEntries(r.Context(), limit)
	if err != nil {
		writeInternal(w, "list audit", err)
		return
	}
	out := make([]auditResponse, len(entries))
	for i, e := range entries {
		out[i] = auditResponse{
			ID: e.ID, ActorUserID: e.ActorUserID, ActorEmail: e.ActorEmail,
			Action: e.Action, Target: e.Target,
			CreatedAt: e.CreatedAt,
		}
		// Pass-through stored JSON as-is. Empty string stays nil so the
		// field can be omitted on the wire.
		if e.Details != "" {
			out[i].Details = json.RawMessage(e.Details)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, map[string]string{"error": code, "detail": detail})
}

func writeInternal(w http.ResponseWriter, op string, err error) {
	slog.Error("admin: "+op, "err", err)
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

func parseID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_id", "id must be a positive integer")
		return 0, false
	}
	return id, true
}

// formatPrice renders kopecks as "250.00" UAH-flavoured. CSV consumers
// (Excel, Google Sheets) parse this as a number when the comma below
// matches the locale, but a dot is the safer default for sql/jq users.
func formatPrice(kopecks int64) string {
	major := kopecks / 100
	minor := kopecks % 100
	if minor < 0 {
		minor = -minor
	}
	return fmt.Sprintf("%d.%02d", major, minor)
}

func formatNullTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}
