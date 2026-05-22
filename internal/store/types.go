package store

import "time"

// Show is one performance with a date/venue. CreatedAt/ArchivedAt are
// populated for shows created through the multi-show API; legacy shows
// migrated from earlier schemas may have a zero CreatedAt. Slug is the
// URL-safe handle used in public-side links (`/event/<slug>`); unique
// across the install. Description is free-form text shown to buyers on
// the public event page. PosterURL is a https://... image link used as
// landing card background and event hero — empty falls back to gradient.
type Show struct {
	ID          int64
	Slug        string
	Title       string
	Venue       string
	StartsAt    time.Time
	Description string
	PosterURL   string
	CreatedAt   time.Time
	ArchivedAt  *time.Time
	// Kind is "seated" (default — buyers pick from a seat map) or "ga"
	// (general admission — buyers pick a quantity, server auto-allocates
	// from a pool of virtual seats laid out in a single row).
	Kind string
	// GACapacity is the original GA pool size at creation time. Used for
	// display only; the live "free" count comes from seat statuses.
	GACapacity int
}

// IsGA returns true for general-admission shows (no seat map).
func (s Show) IsGA() bool { return s.Kind == "ga" }

// Seat is one location in a hall. Row/Col stay the human-friendly grid
// coordinates and remain UNIQUE(show_id, row, col). X/Y are the canvas
// coordinates used by the visual layout editor — for legacy shows they're
// backfilled from row/col on migration. Label overrides the default
// "Ряд R · місце C" rendering when non-empty. Category groups seats for
// styling/pricing in the editor. Sellable=false marks aisles or technical
// slots that the layout shows but the bot refuses to reserve.
type Seat struct {
	ID           int64
	ShowID       int64
	Row, Col     int
	X, Y         float64
	Label        string
	Category     string
	PriceKopecks int64
	Sellable     bool
}

// Reservation is a hold placed on a seat. It becomes a ticket once
// ConfirmedAt is set. CancelledAt is populated on soft-delete (user
// cancel, sweeper expiry, admin force-cancel) and surfaced through
// admin queries so the dashboard can show cancellation history.
// BuyerEmail is set for web-buyer reservations only; bot reservations
// leave it empty and rely on Telegram for delivery.
type Reservation struct {
	ID         int64
	SeatID     int64
	TGUserID   int64
	TGChatID   int64
	BuyerName  string
	BuyerEmail string
	// AttendeeName is the optional per-ticket name shown on the PDF. Empty
	// means "same as BuyerName" — the renderer falls back accordingly.
	// Only the multi-seat web flow lets buyers fill this in; the bot and
	// single-seat web both leave it empty.
	AttendeeName string
	Code         string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	ConfirmedAt  *time.Time
	CancelledAt  *time.Time
	// RefundedAt records that admin manually returned this ticket's
	// money in monobank. Per-seat granularity matters for multi-seat
	// orders where only some attendees ask for a refund.
	RefundedAt *time.Time
}

// Ticket is the QR-bearing artifact issued after a confirmed payment.
type Ticket struct {
	ID            int64
	ReservationID int64
	QRPayload     string
	IssuedAt      time.Time
	UsedAt        *time.Time
}

// Order groups one or more reservations under a single payment. Buyer
// fields, Telegram-chat link, and totals live here — Reservation just
// points at a seat. One pay code (in monobank comment) confirms the
// whole order in one shot, producing N PDFs.
//
// Single-seat reservations created before multi-seat were migrated into
// 1-row orders so the rest of the code can treat everything uniformly.
type Order struct {
	ID           int64
	Code         string
	BuyerName    string
	BuyerEmail   string
	TGUserID     int64
	TGChatID     int64
	TotalKopecks int64
	CreatedAt    time.Time
	ExpiresAt    time.Time
	ConfirmedAt  *time.Time
	CancelledAt  *time.Time
	RemindedAt   *time.Time
	// RefundedAt records that admin manually returned the money in
	// monobank. Doesn't affect seat status (use AdminCancelReservation
	// to actually free the seat); refund_at is purely bookkeeping so the
	// organizer can answer "did I give this person their money back".
	RefundedAt *time.Time
}

// SeatStatus is one of "free", "held", "sold".
type SeatStatus string

const (
	SeatFree SeatStatus = "free"
	SeatHeld SeatStatus = "held"
	SeatSold SeatStatus = "sold"
)

// MyItem couples a user's reservation with its seat for /my output.
// Refund state now lives on Reservation.RefundedAt directly (per-seat
// granularity); MyItem stays a plain pair.
type MyItem struct {
	Reservation Reservation
	Seat        Seat
}

// Stats is an admin snapshot of a single show.
type Stats struct {
	Total          int
	Sold           int
	Held           int
	Free           int
	RevenueKopecks int64
}

// SeatPatch is a partial update for one seat, used by the layout editor.
// Zero-valued pointer fields mean "leave as is".
type SeatPatch struct {
	ID           int64
	X, Y         *float64
	Label        *string
	Category     *string
	PriceKopecks *int64
	Sellable     *bool
}

// NewSeat is the input shape for AddSeat — everything except the auto id.
type NewSeat struct {
	ShowID       int64
	Row, Col     int
	X, Y         float64
	Label        string
	Category     string
	PriceKopecks int64
	Sellable     bool
}

// User is an admin account that can log in to the management web UI.
// PasswordHash is a bcrypt hash — never store or transmit the plaintext.
type User struct {
	ID           int64
	Email        string
	PasswordHash string
	Name         string
	CreatedAt    time.Time
}

// SeatCategory is the admin's named pricing tier — "VIP", "Standard",
// "Balcony" — bound to one show. Seats reference it by Category name
// (string); the matching row here provides the default price and the
// SVG colour the buyer map renders for that group.
//
// PriceKopecks here is the canonical price for the tier; when admin
// edits a category, every seat with the matching name gets a batch
// UPDATE so per-seat price stays in sync. A seat can still hold its
// own different price (e.g. a deliberate odd-spot), but admin should
// just edit it separately.
type SeatCategory struct {
	ID           int64
	ShowID       int64
	Name         string
	Color        string // CSS colour, e.g. "#3b82f6"
	PriceKopecks int64
	SortOrder    int
}

// BuyerTicketRow is one ticket-shaped row returned by BuyerTicketsByEmail.
// Each row carries everything needed to render the buyer's "Мої квитки"
// page: order context, show context, reservation+seat, and ticket
// metadata when issued. Flat (not nested) so the SQL can deliver it in
// one query — the caller groups by Order.ID for display.
type BuyerTicketRow struct {
	Order       Order
	Show        Show
	Reservation Reservation
	Seat        Seat
	// QRPayload + UsedAt come from the tickets table. Both zero-valued
	// when the order hasn't been confirmed yet (no ticket row exists).
	QRPayload string
	UsedAt    *time.Time
}

// AuditEntry is one admin action recorded in the audit_log table.
// ActorEmail is denormalised at write time so a future user deletion
// doesn't blank the trail. Details is a free-form JSON blob — callers
// keep the schema light, the column carries whatever context the
// action needed.
type AuditEntry struct {
	ID           int64
	ActorUserID  int64
	ActorEmail   string
	Action       string // e.g. "show.create", "reservation.cancel"
	Target       string // e.g. "show:42", "reservation:123"
	Details      string // JSON, may be ""
	CreatedAt    time.Time
}

// WaitlistEntry is one buyer waiting for a seat to free up on a show.
// notified_at flips when the freed-seat email goes out; the row stays
// in the table so the same email can't re-subscribe on the same show
// (and so admin can audit who got notified).
type WaitlistEntry struct {
	ID         int64
	ShowID     int64
	Email      string
	CreatedAt  time.Time
	NotifiedAt *time.Time
}

// DailySales is one row of the per-day rollup used by /admin/analytics.
// Date is the local "YYYY-MM-DD" derived from orders.confirmed_at
// (interpreted as UTC by SQLite). Tickets is the count of confirmed
// reservations (i.e. PDF tickets generated); Revenue is the kopecks
// that day brought in.
type DailySales struct {
	Date           string
	Tickets        int
	RevenueKopecks int64
}

// ConversionStats summarises how many orders created in a period
// actually got paid. Useful as a single KPI on the analytics page.
type ConversionStats struct {
	TotalOrders int
	PaidOrders  int
}

// Organizer is the single-row profile that the public /about page
// renders: name, bio, socials, contact. Used as a soft footer on
// event pages so buyers know who's behind the event. All fields are
// optional — empty Organizer means /about renders a generic "Цей
// інстанс ще не налаштований" stub.
type Organizer struct {
	Name         string
	Bio          string
	ContactEmail string
	Phone        string
	WebsiteURL   string
	TelegramURL  string
	InstagramURL string
	FacebookURL  string
	LogoURL      string
	UpdatedAt    time.Time
}

// IsEmpty reports whether the organizer profile has any user-visible
// fields filled in. Public /about uses this to decide between a real
// rendering and a "не налаштовано" stub.
func (o Organizer) IsEmpty() bool {
	return o.Name == "" && o.Bio == "" && o.ContactEmail == "" &&
		o.Phone == "" && o.WebsiteURL == "" && o.TelegramURL == "" &&
		o.InstagramURL == "" && o.FacebookURL == "" && o.LogoURL == ""
}

// Session is a long-lived auth credential keyed by an opaque random Token.
// Issued on successful login, looked up on every authed request, deleted
// on logout. The token lives in an HttpOnly cookie on the client; the
// row in this table is the server-side truth.
type Session struct {
	ID        int64
	UserID    int64
	Token     string
	CreatedAt time.Time
	ExpiresAt time.Time
}
