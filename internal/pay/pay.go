// Package pay glues the mono webhook to the rest of the app: match a
// transfer against an open reservation by code, confirm it, render the
// ticket PDF and send it to the buyer.
package pay

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/OlexiyOdarchuk/go-monobank-sdk/bank"
	"github.com/OlexiyOdarchuk/go-monobank-sdk/money"
	"github.com/OlexiyOdarchuk/go-monobank-sdk/webhook"

	"github.com/OlexiyOdarchuk/monokasa/internal/metrics"
)

// Show is the subset of show info this package passes through to the renderer.
type Show struct {
	Title    string
	Venue    string
	StartsAt time.Time
}

// Seat is the subset of seat info this package passes through.
type Seat struct {
	ID       int64
	Row, Col int
	Price    money.Money
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
	Store    Store
	Coder    Coder
	Notifier Notifier
	Renderer Renderer
	Show     Show
	// MinPrice is the minimum acceptable payment. Overpayment (paid >
	// MinPrice) is fine — the ticket is still issued.
	MinPrice money.Money
}

// Handle is the OnEvent callback wired into webhook.NewHandler.
func (p *Processor) Handle(ctx context.Context, e *webhook.Response) error {
	matched, err := p.processTx(ctx, e.Data.Transaction)
	if matched {
		metrics.IssuedFromWebhook()
	}
	return err
}

// ReconcileTx replays one transaction through the matching logic — used
// by the admin /reconcile command to catch webhooks Mono dropped. Returns
// true if the transaction issued a ticket on this call.
func (p *Processor) ReconcileTx(ctx context.Context, tx bank.Transaction) (bool, error) {
	matched, err := p.processTx(ctx, tx)
	if matched {
		metrics.IssuedFromReconcile()
	}
	return matched, err
}

func (p *Processor) processTx(ctx context.Context, t bank.Transaction) (bool, error) {
	if t.Amount.Minor <= 0 {
		return false, nil // outflow, not interesting
	}

	code := extractCode(t.Comment, t.Description)
	if code == "" {
		slog.Info("payment with no reservation code in comment",
			"txId", t.ID, "comment", t.Comment)
		return false, nil
	}
	res, seat, err := p.Store.FindReservationByCode(ctx, code)
	if errors.Is(err, ErrCodeNotFound) || errors.Is(err, ErrAlreadyClosed) {
		slog.Info("payment code has no open reservation",
			"txId", t.ID, "code", code)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if res.ConfirmedAt != nil {
		slog.Info("payment for already-confirmed reservation",
			"txId", t.ID, "code", code)
		return false, nil
	}
	// Accept paid >= expected: overpayment is fine, exact match is fine,
	// short payment is rejected.
	if t.Amount.Minor < p.MinPrice.Minor {
		slog.Warn("payment short",
			"txId", t.ID, "paid", t.Amount, "need", p.MinPrice, "code", code)
		return false, nil
	}

	qrPayload := p.Coder.QRPayload(res.ID, seat.ID)
	if err := p.Store.Confirm(ctx, res.ID, qrPayload); err != nil {
		return false, err
	}
	pdf, err := p.Renderer(p.Show, seat, res.BuyerName, qrPayload)
	if err != nil {
		return false, err
	}
	if err := p.Notifier.SendTicket(res.TGChatID, seat, pdf); err != nil {
		return false, err
	}
	slog.Info("ticket issued",
		"code", code,
		"row", seat.Row,
		"seat", seat.Col,
		"buyer", res.BuyerName,
		"chatId", res.TGChatID,
		"paid", t.Amount)
	return true, nil
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
