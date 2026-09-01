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

	"crypto/ecdsa"
	"sync"

	"github.com/OlexiyOdarchuk/go-monobank-sdk/v2/acquiring"
	"github.com/OlexiyOdarchuk/go-monobank-sdk/v2/bank"
	"github.com/OlexiyOdarchuk/go-monobank-sdk/v2/currency"
	"github.com/OlexiyOdarchuk/go-monobank-sdk/v2/money"
	"github.com/OlexiyOdarchuk/go-monobank-sdk/v2/webhook"

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
	// FindOrderByInvoiceID is the acquiring dual of FindOrderByCode —
	// the webhook arrives with monobank's invoice id, not our code,
	// so the matcher needs this lookup. Returns ErrCodeNotFound when
	// nothing matches (unsolicited webhook for a stale invoice).
	FindOrderByInvoiceID(ctx context.Context, invoiceID string) (Order, []OrderItem, error)
	// FindShowByID resolves the show that owns a given seat. Used at
	// confirmation time so the PDF/email picks up the order's actual
	// show, not whatever ActiveShow happens to return (which can
	// drift between orders for sites with multiple concurrent shows).
	FindShowByID(ctx context.Context, showID int64) (Show, error)
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
	// AcquiringClient is the monobank Merchant API client. Optional —
	// when nil, shows with payment_method='acquiring' fall back to jar
	// at handler level (see public.createOrder). When set, the
	// processor knows how to create invoices and verify webhooks.
	AcquiringClient *acquiring.Client
	// BaseURL is the public origin of this deploy ("https://kasa.x.com").
	// Used to compose redirectUrl + webHookUrl on invoice create so
	// monobank can call us back. Mandatory when AcquiringClient is set.
	BaseURL string

	// Acquiring webhook signing key, fetched lazily from monobank
	// (/api/merchant/pubkey) on first use and cached. Rotates rarely
	// enough that one round-trip per process lifetime is fine.
	acqKeyOnce sync.Once
	acqKey     *ecdsa.PublicKey
	acqKeyErr  error
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
	// Resolve the show from the order's actual seats, not the globally-
	// active show. Without this, PDFs (and the success-email) for an
	// older event picked up the title/venue of whatever show happened
	// to be "active" at confirmation time.
	if len(items) == 0 {
		return fmt.Errorf("deliver: order %d has no items", order.ID)
	}
	show, err := p.Store.FindShowByID(ctx, items[0].Seat.ShowID)
	if err != nil {
		return fmt.Errorf("resolve show %d: %w", items[0].Seat.ShowID, err)
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

// --- acquiring (monobank Merchant API) ---

// AcquiringEnabled reports whether the processor was configured with a
// merchant API client. Public handlers branch on this when deciding
// whether to honour show.payment_method='acquiring' or fall back to jar.
func (p *Processor) AcquiringEnabled() bool { return p.AcquiringClient != nil }

// CreateAcquiringInvoice asks monobank to mint a real invoice for the
// order, returning the URL the buyer should be redirected to. The
// invoice ID is stored on the order so the webhook handler can match
// the eventual "success" notification back to it.
//
// redirectURL is the buyer-facing page to land on after payment (the
// event page); webhookURL is what monobank POSTs to with the status
// update. Both should be absolute https URLs.
func (p *Processor) CreateAcquiringInvoice(ctx context.Context, order Order, items []OrderItem, showTitle string) (pageURL string, invoiceID string, err error) {
	if p.AcquiringClient == nil {
		return "", "", fmt.Errorf("acquiring not configured")
	}
	if p.BaseURL == "" {
		return "", "", fmt.Errorf("acquiring requires BaseURL on processor")
	}
	// Build line-items: monobank likes a description and per-item
	// breakdown. We send one line per reservation seat — keeps the
	// receipt readable.
	basket := make([]acquiring.BasketItem, 0, len(items))
	for _, it := range items {
		name := fmt.Sprintf("%s · ряд %d місце %d", showTitle, it.Seat.Row, it.Seat.Col)
		if it.Seat.Category == "GA" {
			name = fmt.Sprintf("%s · GA #%d", showTitle, it.Seat.Col)
		}
		basket = append(basket, acquiring.BasketItem{
			Name: name,
			Qty:  1,
			Sum:  it.Seat.Price.Minor,
			Unit: "шт.",
			Code: fmt.Sprintf("res-%d", it.ReservationID),
		})
	}
	resp, err := p.AcquiringClient.CreateInvoice(ctx, &acquiring.CreateInvoiceRequest{
		Amount:   order.TotalKopecks,
		Currency: currency.UAH,
		MerchantPaymInfo: &acquiring.MerchantPaymInfo{
			Reference:   order.Code,
			Destination: fmt.Sprintf("Квитки на %q · код %s", showTitle, order.Code),
			BasketOrder: basket,
		},
		RedirectURL: p.BaseURL + "/event/by-code/" + order.Code,
		WebHookURL:  p.BaseURL + "/webhook/acquiring",
		// Validity matches HOLD duration so monobank invalidates the
		// invoice when our reservation would expire anyway.
		Validity: int64((15 * time.Minute).Seconds()),
	})
	if err != nil {
		return "", "", fmt.Errorf("create invoice: %w", err)
	}
	return resp.PageURL, resp.InvoiceID, nil
}

// HandleAcquiringWebhook verifies the signed POST monobank sent us,
// parses the payload, and confirms+delivers the matching order when
// status reaches "success". Returns nil for status updates we don't
// act on (created/processing/hold) so the caller can return 200 — the
// bank retries non-2xx responses.
func (p *Processor) HandleAcquiringWebhook(ctx context.Context, body []byte, xSign string) error {
	pub, err := p.acquiringPubKey(ctx)
	if err != nil {
		return fmt.Errorf("pubkey: %w", err)
	}
	if err := acquiring.VerifyWebhook(pub, body, xSign); err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	hook, err := acquiring.ParseWebhook(body)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	switch hook.Status {
	case acquiring.InvoiceSuccess:
		// Fall through to confirmation.
	case acquiring.InvoiceFailure, acquiring.InvoiceExpired, acquiring.InvoiceReversed:
		slog.Info("acquiring webhook: non-success status",
			"invoiceId", hook.InvoiceID, "status", hook.Status,
			"reason", hook.FailureReason)
		return nil
	default:
		// created / processing / hold — nothing to do yet.
		return nil
	}
	order, items, err := p.Store.FindOrderByInvoiceID(ctx, hook.InvoiceID)
	if err != nil {
		return fmt.Errorf("find order by invoice %q: %w", hook.InvoiceID, err)
	}
	if order.ConfirmedAt != nil {
		// Idempotent — monobank retries until they get a 200.
		return nil
	}
	return p.deliverConfirmedOrder(ctx, order, items, deliverContext{
		AuditAction: "payment.acquiring",
		AuditExtra: map[string]any{
			"invoice_id":     hook.InvoiceID,
			"final_kopecks":  hook.FinalAmount.Minor,
			"merchant_paid":  hook.Amount.Minor,
		},
	})
}

// acquiringPubKey lazily fetches the merchant signing key from monobank
// on first use and caches it for the rest of the process lifetime.
// Concurrent callers join via sync.Once; failure is sticky (the
// process needs a restart or fresh PubKey call to recover).
func (p *Processor) acquiringPubKey(ctx context.Context) (*ecdsa.PublicKey, error) {
	p.acqKeyOnce.Do(func() {
		if p.AcquiringClient == nil {
			p.acqKeyErr = fmt.Errorf("acquiring client not configured")
			return
		}
		sk, err := p.AcquiringClient.PubKey(ctx)
		if err != nil {
			p.acqKeyErr = fmt.Errorf("fetch pubkey: %w", err)
			return
		}
		pub, err := acquiring.ParsePubKey([]byte(sk.Key))
		if err != nil {
			p.acqKeyErr = fmt.Errorf("parse pubkey: %w", err)
			return
		}
		p.acqKey = pub
	})
	return p.acqKey, p.acqKeyErr
}
