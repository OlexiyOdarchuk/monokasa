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
}

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
}

// SeatStatus is one of "free", "held", "sold".
type SeatStatus string

const (
	SeatFree SeatStatus = "free"
	SeatHeld SeatStatus = "held"
	SeatSold SeatStatus = "sold"
)

// MyItem couples a user's reservation with its seat for /my output.
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
