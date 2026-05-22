// Package pay glues the mono webhook to the rest of the app: match the
// short code in a transfer comment against an open Order, confirm the
// whole order (one or N seats), render PDFs and ship them to the buyer
// over Telegram and/or email.
//
// Pre-multi-seat single-seat reservations are migrated into 1-row
// orders by store.Open, so this package has only one lookup path now.
package pay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"time"

	"github.com/OlexiyOdarchuk/go-monobank-sdk/bank"
	"github.com/OlexiyOdarchuk/go-monobank-sdk/money"
	"github.com/OlexiyOdarchuk/go-monobank-sdk/webhook"

	"github.com/OlexiyOdarchuk/monokasa/internal/metrics"
	"github.com/OlexiyOdarchuk/monokasa/internal/realtime"
)

// Show is the subset of show info this package passes through to the
// renderer and email side. Slug is used as the stable iCalendar UID
// seed so re-sending an invite updates the buyer's calendar entry
// instead of duplicating it.
type Show struct {
	Slug     string
	Title    string
	Venue    string
	StartsAt time.Time
}

// Seat is the subset of seat info this package passes through. Category
// flows through to the PDF renderer so GA tickets can drop row/col in
// favour of "Квиток №N".
type Seat struct {
	ID       int64
	ShowID   int64
	Row, Col int
	Category string
	Price    money.Money
}

// Order is the subset of order info the processor reasons about.
type Order struct {
	ID              int64
	Code            string
	BuyerName       string
	BuyerEmail      string
	TGChatID        int64
	TotalKopecks    int64
	DiscountCode    string
	DiscountKopecks int64
	ConfirmedAt     *time.Time
}

// OrderItem is one seat inside an order. ReservationID is what we mint
// the QR payload for (each PDF carries one reservation's QR).
// AttendeeName is the per-ticket name (empty → fall back to the order's
// BuyerName at render time).
type OrderItem struct {
	ReservationID int64
	AttendeeName  string
	Seat          Seat
}

// Domain errors the processor expects the Store to return.
var (
	ErrCodeNotFound  = errors.New("order code not found")
	ErrAlreadyClosed = errors.New("order already closed")
)

// Store is the persistence behavior the processor needs.
type Store interface {
	FindOrderByCode(ctx context.Context, code string) (Order, []OrderItem, error)
	ConfirmOrder(ctx context.Context, orderID int64, qrPayloads map[int64]string) error
	// LogAudit appends one row to the audit_log table. Pay processor
	// logs payment.confirm here so the journal shows monobank webhook
	// activity alongside admin actions. Failures get swallowed at the
	// call site — losing an audit row never blocks ticket delivery.
	LogAudit(ctx context.Context, action, target, actorLabel, detailsJSON string) error
}

// Coder mints the signed QR payload embedded in each issued ticket.
type Coder interface {
	QRPayload(reservationID, seatID int64) string
}

// Notifier delivers a ticket PDF over Telegram. Called once per seat.
type Notifier interface {
	SendTicket(chatID int64, seat Seat, pdf []byte) error
}

// EmailItem couples a seat with its rendered PDF for batch email delivery.
type EmailItem struct {
	Seat Seat
	PDF  []byte
}

// EmailDelivery ships tickets over SMTP. SendTicketBatchEmail takes all
// items in one message (single email, N attachments). Cancellation is a
// separate channel — body and no attachments.
type EmailDelivery interface {
	SendTicketBatchEmail(ctx context.Context, to, buyerName string, items []EmailItem, show Show) error
	SendCancellationEmail(ctx context.Context, to, buyerName string, seat Seat, show Show) error
}

// Renderer turns a confirmed reservation into a printable PDF.
type Renderer func(show Show, seat Seat, buyerName, qrPayload string) ([]byte, error)

// ShowFn resolves the currently active show on each payment.
type ShowFn func(ctx context.Context) (Show, error)

type Processor struct {
	Store    Store
	Coder    Coder
	Notifier Notifier
	Renderer Renderer
	ShowFn   ShowFn
	// Email is optional. When nil, web-buyer reservations confirm but
	// the buyer doesn't get an email — operator log will warn.
	Email EmailDelivery
	// Hub is optional. When set, every confirmed seat gets a
	// realtime.SeatSold event so live SSE subscribers update without
	// reloading. Nil-safe (Publish on a nil hub is a no-op).
	Hub *realtime.Hub
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
// by the admin /reconcile command to catch webhooks Mono dropped.
func (p *Processor) ReconcileTx(ctx context.Context, tx bank.Transaction) (bool, error) {
	matched, err := p.processTx(ctx, tx)
	if matched {
		metrics.IssuedFromReconcile()
	}
	return matched, err
}

// ConfirmFreeOrder is the no-webhook path for orders whose post-discount
// total is zero — a 100%-off promo, comp ticket, etc. monobank can't
// issue 0-kopeck invoices and the regular processTx requires
// Amount > 0, so the public handler calls this synchronously right
// after CreateOrderWithDiscount and the buyer drops straight onto the
// "paid" screen.
//
// Behaviour mirrors processTx after the amount check: load order,
// confirm in store (writes Ticket rows + QRs), broadcast SSE, audit,
// render PDFs, deliver via Telegram + email. Returns the items so the
// caller can include them in the API response if it wants.
func (p *Processor) ConfirmFreeOrder(ctx context.Context, code string) ([]OrderItem, error) {
	order, items, err := p.Store.FindOrderByCode(ctx, code)
	if err != nil {
		return nil, err
	}
	if order.TotalKopecks != 0 {
		return nil, fmt.Errorf("ConfirmFreeOrder: order %q total %d != 0", code, order.TotalKopecks)
	}
	if order.ConfirmedAt != nil {
		// Already confirmed — idempotent return, no double-delivery.
		return items, nil
	}
	if err := p.deliverConfirmedOrder(ctx, order, items, deliverContext{
		AuditAction: "payment.free",
		AuditExtra: map[string]any{
			"discount_code":    order.DiscountCode,
			"discount_kopecks": order.DiscountKopecks,
		},
	}); err != nil {
		return nil, err
	}
	metrics.IssuedFromWebhook() // same bucket — semantically "issued out-of-band"
	return items, nil
}

// deliverContext bundles the metadata that differs between confirm
// callers (webhook vs free-order) so the shared delivery code below
// stays caller-agnostic.
type deliverContext struct {
	AuditAction string         // e.g. "payment.confirm" or "payment.free"
	AuditExtra  map[string]any // merged into the audit details blob
}

// deliverConfirmedOrder is the shared confirm+render+deliver path used
// by both webhook (processTx) and the no-webhook free-order branch.
// Caller has already loaded the order and verified eligibility.
func (p *Processor) deliverConfirmedOrder(ctx context.Context, order Order, items []OrderItem, dc deliverContext) error {
	show, err := p.ShowFn(ctx)
	if err != nil {
		return fmt.Errorf("resolve show: %w", err)
	}
	qrs := make(map[int64]string, len(items))
	for _, it := range items {
		qrs[it.ReservationID] = p.Coder.QRPayload(it.ReservationID, it.Seat.ID)
	}
	if err := p.Store.ConfirmOrder(ctx, order.ID, qrs); err != nil {
		return err
	}
	for _, it := range items {
		p.Hub.Publish(it.Seat.ShowID, realtime.Event{
			Type: "seat_status", SeatID: it.Seat.ID, Status: realtime.SeatSold,
		})
	}
	details := map[string]any{
		"code": order.Code, "seats": len(items),
		"total_kopecks": order.TotalKopecks,
	}
	maps.Copy(details, dc.AuditExtra)
	auditJSON, _ := json.Marshal(details)
	if err := p.Store.LogAudit(ctx, dc.AuditAction,
		fmt.Sprintf("order:%d", order.ID), order.BuyerEmail, string(auditJSON)); err != nil {
		slog.Error("audit write failed", "code", order.Code, "err", err)
	}
	pdfs := make([][]byte, 0, len(items))
	for _, it := range items {
		name := it.AttendeeName
		if name == "" {
			name = order.BuyerName
		}
		pdf, err := p.Renderer(show, it.Seat, name, qrs[it.ReservationID])
		if err != nil {
			slog.Error("render pdf", "code", order.Code, "seatId", it.Seat.ID, "err", err)
			continue
		}
		pdfs = append(pdfs, pdf)
	}
	if order.TGChatID != 0 {
		for i, it := range items {
			if i >= len(pdfs) {
				break
			}
			if err := p.Notifier.SendTicket(order.TGChatID, it.Seat, pdfs[i]); err != nil {
				return err
			}
		}
	}
	if order.BuyerEmail != "" && p.Email != nil {
		batch := make([]EmailItem, 0, len(pdfs))
		for i, it := range items {
			if i >= len(pdfs) {
				break
			}
			batch = append(batch, EmailItem{Seat: it.Seat, PDF: pdfs[i]})
		}
		if err := p.Email.SendTicketBatchEmail(ctx, order.BuyerEmail, order.BuyerName, batch, show); err != nil {
			slog.Error("send ticket batch email",
				"code", order.Code, "to", order.BuyerEmail, "err", err)
		}
	}
	slog.Info("order confirmed",
		"code", order.Code, "seats", len(items),
		"buyer", order.BuyerName, "email", order.BuyerEmail,
		"chatId", order.TGChatID, "total", order.TotalKopecks,
		"path", dc.AuditAction)
	return nil
}

func (p *Processor) processTx(ctx context.Context, t bank.Transaction) (bool, error) {
	if t.Amount.Minor <= 0 {
		return false, nil // outflow, not interesting
	}

	code := extractCode(t.Comment, t.Description)
	if code == "" {
		slog.Info("payment with no order code in comment",
			"txId", t.ID, "comment", t.Comment)
		return false, nil
	}
	order, items, err := p.Store.FindOrderByCode(ctx, code)
	if errors.Is(err, ErrCodeNotFound) || errors.Is(err, ErrAlreadyClosed) {
		slog.Info("payment code has no open order",
			"txId", t.ID, "code", code)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if order.ConfirmedAt != nil {
		slog.Info("payment for already-confirmed order",
			"txId", t.ID, "code", code)
		return false, nil
	}
	// Accept paid >= total: overpayment is fine, exact match is fine,
	// short payment is rejected.
	if t.Amount.Minor < order.TotalKopecks {
		slog.Warn("payment short",
			"txId", t.ID, "paid", t.Amount, "need", order.TotalKopecks, "code", code)
		return false, nil
	}

	if err := p.deliverConfirmedOrder(ctx, order, items, deliverContext{
		AuditAction: "payment.confirm",
		AuditExtra: map[string]any{
			"paid_kopecks": t.Amount.Minor,
			"tx_id":        t.ID,
		},
	}); err != nil {
		return false, err
	}
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
