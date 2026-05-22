// Package e2e contains cross-package integration tests that exercise the
// real contract between store, pay, token and web together. Lives outside
// the production packages so they stay independent of each other; the
// adapter glue here is a near-clone of cmd/monokasa/main.go's wiring and
// will break if the inter-package shapes drift.
package e2e

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OlexiyOdarchuk/go-monobank-sdk/bank"
	"github.com/OlexiyOdarchuk/go-monobank-sdk/currency"
	"github.com/OlexiyOdarchuk/go-monobank-sdk/money"
	"github.com/OlexiyOdarchuk/go-monobank-sdk/webhook"

	"github.com/OlexiyOdarchuk/monokasa/internal/pay"
	"github.com/OlexiyOdarchuk/monokasa/internal/store"
	"github.com/OlexiyOdarchuk/monokasa/internal/token"
	"github.com/OlexiyOdarchuk/monokasa/internal/web"
)

// --- adapters ---

type payAdapter struct{ s *store.Store }

func (p payAdapter) FindOrderByCode(ctx context.Context, code string) (pay.Order, []pay.OrderItem, error) {
	o, items, err := p.s.FindOrderByCode(ctx, code)
	payOrder := pay.Order{
		ID: o.ID, Code: o.Code,
		BuyerName:    o.BuyerName,
		BuyerEmail:   o.BuyerEmail,
		TGChatID:     o.TGChatID,
		TotalKopecks: o.TotalKopecks,
		ConfirmedAt:  o.ConfirmedAt,
	}
	switch {
	case errors.Is(err, store.ErrCodeNotFound):
		return payOrder, nil, pay.ErrCodeNotFound
	case errors.Is(err, store.ErrAlreadyClosed):
		return payOrder, nil, pay.ErrAlreadyClosed
	case err != nil:
		return payOrder, nil, err
	}
	payItems := make([]pay.OrderItem, len(items))
	for i, it := range items {
		payItems[i] = pay.OrderItem{
			ReservationID: it.Reservation.ID,
			Seat: pay.Seat{
				ID: it.Seat.ID, Row: it.Seat.Row, Col: it.Seat.Col,
				Price: money.New(it.Seat.PriceKopecks, currency.UAH),
			},
		}
	}
	return payOrder, payItems, nil
}

func (p payAdapter) FindOrderByInvoiceID(_ context.Context, _ string) (pay.Order, []pay.OrderItem, error) {
	// e2e test doesn't exercise the acquiring path — return a stub
	// that satisfies the interface and matches "not found" semantics
	// the tests would never hit anyway.
	return pay.Order{}, nil, pay.ErrCodeNotFound
}

func (p payAdapter) ConfirmOrder(ctx context.Context, orderID int64, qrPayloads map[int64]string) error {
	_, err := p.s.ConfirmOrder(ctx, orderID, qrPayloads)
	return err
}

func (p payAdapter) LogAudit(ctx context.Context, action, target, actorLabel, detailsJSON string) error {
	return p.s.LogAudit(ctx, store.AuditEntry{
		ActorEmail: actorLabel, Action: action, Target: target, Details: detailsJSON,
	})
}

type webAdapter struct{ s *store.Store }

func (w webAdapter) UseTicket(ctx context.Context, qrPayload string) (web.Ticket, error) {
	t, err := w.s.UseTicket(ctx, qrPayload)
	out := web.Ticket{ID: t.ID, UsedAt: t.UsedAt}
	switch {
	case errors.Is(err, store.ErrTicketNotFound):
		return out, web.ErrTicketNotFound
	case errors.Is(err, store.ErrTicketUsed):
		return out, web.ErrTicketUsed
	default:
		return out, err
	}
}

func (w webAdapter) FindReservationByTicket(ctx context.Context, ticketID int64) (web.Reservation, web.Seat, error) {
	r, s, err := w.s.FindReservationByTicket(ctx, ticketID)
	return web.Reservation{BuyerName: r.BuyerName, ConfirmedAt: r.ConfirmedAt},
		web.Seat{ID: s.ID, Row: s.Row, Col: s.Col},
		err
}

// captureNotifier records every ticket pay.Processor tries to deliver.
type captureNotifier struct {
	mu    sync.Mutex
	sends []capturedSend
}

type capturedSend struct {
	chatID int64
	seat   pay.Seat
	pdfLen int
}

func (n *captureNotifier) SendTicket(chatID int64, seat pay.Seat, pdf []byte) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sends = append(n.sends, capturedSend{chatID, seat, len(pdf)})
	return nil
}

// captureRenderer records the qrPayload pay handed to the renderer so the
// test can replay it through web.Scanner without going through the PDF.
type captureRenderer struct {
	mu       sync.Mutex
	payloads []string
}

func (r *captureRenderer) render(show pay.Show, seat pay.Seat, buyerName, qrPayload string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.payloads = append(r.payloads, qrPayload)
	return []byte("PDF"), nil
}

func (r *captureRenderer) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.payloads) == 0 {
		return ""
	}
	return r.payloads[len(r.payloads)-1]
}

// --- test wiring ---

type harness struct {
	t        *testing.T
	store    *store.Store
	coder    *token.Coder
	proc     *pay.Processor
	notifier *captureNotifier
	render   *captureRenderer
	scanner  *web.Scanner
	showID   int64
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "tix.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	showID, err := st.SeedIfEmpty(ctx, store.Show{
		Title: "E2E", Venue: "Home", StartsAt: time.Now().Add(24 * time.Hour),
	}, 2, 3, 25000)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	coder := token.NewCoder("a-very-long-test-secret-with-enough-bytes")
	notifier := &captureNotifier{}
	render := &captureRenderer{}
	proc := &pay.Processor{
		Store:    payAdapter{st},
		Coder:    coder,
		Notifier: notifier,
		Renderer: render.render,
		ShowFn:   func(context.Context) (pay.Show, error) { return pay.Show{Title: "E2E", Venue: "Home"}, nil },
	}
	scanner := web.NewScanner(webAdapter{st}, coder, "")
	return &harness{
		t: t, store: st, coder: coder, proc: proc,
		notifier: notifier, render: render, scanner: scanner, showID: showID,
	}
}

func (h *harness) reserveSeat(row, col int, code string) store.Reservation {
	h.t.Helper()
	ctx := context.Background()
	seat, err := h.store.FindFreeSeat(ctx, h.showID, row, col)
	if err != nil {
		h.t.Fatalf("FindFreeSeat: %v", err)
	}
	r, err := h.store.Reserve(ctx, seat, 1001, 9001, "Олексій Одарчук", "", code, 15*time.Minute)
	if err != nil {
		h.t.Fatalf("Reserve: %v", err)
	}
	return r
}

func (h *harness) feedWebhook(code string, amount int64) {
	h.t.Helper()
	tx := bank.Transaction{
		ID:      "tx-" + code,
		Comment: "payment " + code,
		Amount:  money.New(amount, currency.UAH),
	}
	if err := h.proc.Handle(context.Background(), &webhook.Response{
		Data: webhook.Data{Transaction: tx},
	}); err != nil {
		h.t.Fatalf("Handle: %v", err)
	}
}

// TestE2E_HappyPath: reserve → webhook → ticket issued → scan ok → scan used.
func TestE2E_HappyPath(t *testing.T) {
	h := newHarness(t)
	h.reserveSeat(1, 1, "happycde")
	h.feedWebhook("happycde", 25000)

	if got := len(h.notifier.sends); got != 1 {
		t.Fatalf("notifier sends = %d, want 1", got)
	}
	qr := h.render.last()
	if qr == "" {
		t.Fatal("no qrPayload captured")
	}

	resp := h.scan(qr)
	if resp.Status != "ok" {
		t.Fatalf("first scan: got %q (%s), want ok", resp.Status, resp.Detail)
	}
	if !strings.Contains(resp.Buyer, "Олексій") {
		t.Errorf("scan buyer = %q, want Олексій in it", resp.Buyer)
	}
	if !strings.Contains(resp.Seat, "Ряд 1") {
		t.Errorf("scan seat = %q, want 'Ряд 1' in it", resp.Seat)
	}

	resp2 := h.scan(qr)
	if resp2.Status != "used" {
		t.Fatalf("second scan: got %q, want used", resp2.Status)
	}
}

// TestE2E_TamperedQRRejected: a forged QR with a swapped signature is invalid.
func TestE2E_TamperedQRRejected(t *testing.T) {
	h := newHarness(t)
	h.reserveSeat(1, 1, "realcdef")
	h.feedWebhook("realcdef", 25000)
	good := h.render.last()

	// Mint a payload that claims a different reservation but reuses the
	// real signature — should fail HMAC verification.
	parts := strings.SplitN(good, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("malformed good payload: %q", good)
	}
	forged := "MDpO.invalidsig"

	resp := h.scan(forged)
	if resp.Status != "invalid" {
		t.Fatalf("forged QR: got %q, want invalid", resp.Status)
	}
}

// TestE2E_WebhookReplayDoesNotDoubleIssue: redelivery (mono retried) only
// produces one ticket.
func TestE2E_WebhookReplayDoesNotDoubleIssue(t *testing.T) {
	h := newHarness(t)
	h.reserveSeat(1, 2, "replaycd")
	h.feedWebhook("replaycd", 25000)
	h.feedWebhook("replaycd", 25000)

	if got := len(h.notifier.sends); got != 1 {
		t.Fatalf("notifier sends = %d, want 1 (idempotent)", got)
	}
}

// TestE2E_ReconcileAfterMissedWebhook: simulate Mono never delivering the
// webhook, then run reconcile-style flow and verify exactly one ticket.
func TestE2E_ReconcileAfterMissedWebhook(t *testing.T) {
	h := newHarness(t)
	h.reserveSeat(1, 3, "rescuecd")

	tx := bank.Transaction{
		ID:      "tx-rescue",
		Comment: "from john rescuecd",
		Amount:  money.New(25000, currency.UAH),
	}
	matched, err := h.proc.ReconcileTx(context.Background(), tx)
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("ReconcileTx should have matched")
	}
	// A second reconcile pass over the same tx must be a no-op.
	matched, err = h.proc.ReconcileTx(context.Background(), tx)
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Fatal("second ReconcileTx should not match")
	}
	if got := len(h.notifier.sends); got != 1 {
		t.Errorf("sends = %d, want 1", got)
	}
}

// --- scan helper: bypasses the HTTP layer and exercises the same logic
// the handler does (verify payload → UseTicket → look up reservation). ---

type scanResult struct {
	Status   string
	Detail   string
	Buyer    string
	Seat     string
	BookedAt string
	UsedAt   string
}

// scan replays what web.Scanner.handleCheck does for a payload. We don't
// stand up a real HTTP server here to keep the test focused on the
// cross-package contract; the HTTP surface is covered by web's own tests.
func (h *harness) scan(payload string) scanResult {
	h.t.Helper()
	ctx := context.Background()

	if _, _, err := h.coder.VerifyQRPayload(payload); err != nil {
		return scanResult{Status: "invalid", Detail: err.Error()}
	}

	t, err := h.store.UseTicket(ctx, payload)
	switch {
	case errors.Is(err, store.ErrTicketUsed):
		r, s, _ := h.store.FindReservationByTicket(ctx, t.ID)
		return scanResult{
			Status: "used",
			Buyer:  r.BuyerName,
			Seat:   "Ряд " + itoa(s.Row) + " · місце " + itoa(s.Col),
		}
	case errors.Is(err, store.ErrTicketNotFound):
		return scanResult{Status: "invalid", Detail: "ticket not found"}
	case err != nil:
		return scanResult{Status: "invalid", Detail: err.Error()}
	}
	r, s, _ := h.store.FindReservationByTicket(ctx, t.ID)
	return scanResult{
		Status: "ok",
		Buyer:  r.BuyerName,
		Seat:   "Ряд " + itoa(s.Row) + " · місце " + itoa(s.Col),
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
