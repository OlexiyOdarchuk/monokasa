// Package pay glues the mono webhook to the rest of the app: match a
// transfer against an open reservation by code, confirm it, render the
// ticket PDF and send it to the buyer.
package pay

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/OlexiyOdarchuk/monosdk"
)

// Show is the subset of show info this package passes through to the renderer.
type Show struct {
	Title    string
	Venue    string
	StartsAt time.Time
}

// Seat is the subset of seat info this package passes through.
type Seat struct {
	ID           int64
	Row, Col     int
	PriceKopecks int64
}

// Reservation is the subset of reservation info the processor needs.
type Reservation struct {
	ID          int64
	TGChatID    int64
	BuyerName   string
	ConfirmedAt *time.Time
}

// Domain errors the processor expects the Store to return.
var (
	ErrCodeNotFound  = errors.New("reservation code not found")
	ErrAlreadyClosed = errors.New("reservation already closed")
)

// Store is the persistence behavior the processor needs.
type Store interface {
	FindReservationByCode(ctx context.Context, code string) (Reservation, Seat, error)
	Confirm(ctx context.Context, reservationID int64, qrPayload string) error
}

// Coder mints the signed QR payload embedded in the issued ticket.
type Coder interface {
	QRPayload(reservationID, seatID int64) string
}

// Notifier delivers the rendered ticket back to the buyer.
type Notifier interface {
	SendTicket(chatID int64, seat Seat, pdf []byte) error
}

// Renderer turns a confirmed reservation into a printable PDF.
type Renderer func(show Show, seat Seat, buyerName, qrPayload string) ([]byte, error)

type Processor struct {
	Store        Store
	Coder        Coder
	Notifier     Notifier
	Renderer     Renderer
	Show         Show
	PriceKopecks int64
}

// Handle is the OnEvent callback wired into monosdk.WebhookHandler.
func (p *Processor) Handle(ctx context.Context, e *monosdk.WebHookResponse) error {
	t := e.Data.Transaction
	if t.Amount <= 0 {
		return nil // outflow, not interesting
	}

	code := extractCode(t.Comment, t.Description)
	if code == "" {
		log.Printf("payment %s: no reservation code in comment %q", t.ID, t.Comment)
		return nil
	}
	res, seat, err := p.Store.FindReservationByCode(ctx, code)
	if errors.Is(err, ErrCodeNotFound) || errors.Is(err, ErrAlreadyClosed) {
		log.Printf("payment %s: code %q has no open reservation", t.ID, code)
		return nil
	}
	if err != nil {
		return err
	}
	if res.ConfirmedAt != nil {
		log.Printf("payment %s: reservation %s already confirmed", t.ID, code)
		return nil
	}
	if t.Amount < p.PriceKopecks {
		log.Printf("payment %s: short amount %d (need %d) for code %s",
			t.ID, t.Amount, p.PriceKopecks, code)
		return nil
	}

	qrPayload := p.Coder.QRPayload(res.ID, seat.ID)
	if err := p.Store.Confirm(ctx, res.ID, qrPayload); err != nil {
		return err
	}
	pdf, err := p.Renderer(p.Show, seat, res.BuyerName, qrPayload)
	if err != nil {
		return err
	}
	if err := p.Notifier.SendTicket(res.TGChatID, seat, pdf); err != nil {
		return err
	}
	log.Printf("ticket issued: code=%s row=%d seat=%d buyer=%q chat=%d",
		code, seat.Row, seat.Col, res.BuyerName, res.TGChatID)
	return nil
}

// extractCode pulls an 8-char base32 reservation code out of a free-form
// comment / description ("just abc12345 by itself" or
// "from john, code: abc12345").
func extractCode(fields ...string) string {
	for _, f := range fields {
		f = strings.ToLower(strings.TrimSpace(f))
		for _, tok := range strings.FieldsFunc(f, func(r rune) bool {
			return !(r >= 'a' && r <= 'z') && !(r >= '2' && r <= '7')
		}) {
			if len(tok) == 8 {
				return tok
			}
		}
	}
	return ""
}
