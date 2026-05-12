package store

import "time"

// Show is one performance with a date/venue.
type Show struct {
	ID       int64
	Title    string
	Venue    string
	StartsAt time.Time
}

// Seat is a single row/column slot within a show, priced in kopecks.
type Seat struct {
	ID           int64
	ShowID       int64
	Row, Col     int
	PriceKopecks int64
}

// Reservation is a hold placed on a seat by a Telegram user. It becomes a
// ticket once ConfirmedAt is set.
type Reservation struct {
	ID          int64
	SeatID      int64
	TGUserID    int64
	TGChatID    int64
	BuyerName   string
	Code        string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	ConfirmedAt *time.Time
}

// Ticket is the QR-bearing artifact issued after a confirmed payment.
type Ticket struct {
	ID            int64
	ReservationID int64
	QRPayload     string
	IssuedAt      time.Time
	UsedAt        *time.Time
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

// Stats is an admin snapshot of the only show.
type Stats struct {
	Total          int
	Sold           int
	Held           int
	Free           int
	RevenueKopecks int64
}
