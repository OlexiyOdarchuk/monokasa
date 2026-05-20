package pay

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/OlexiyOdarchuk/go-monobank-sdk/bank"
	"github.com/OlexiyOdarchuk/go-monobank-sdk/currency"
	"github.com/OlexiyOdarchuk/go-monobank-sdk/money"
	"github.com/OlexiyOdarchuk/go-monobank-sdk/webhook"
)

func TestExtractCode(t *testing.T) {
	cases := []struct {
		name   string
		fields []string
		want   string
	}{
		{"bare", []string{"abcdefgh"}, "abcdefgh"},
		{"lowercased", []string{"ABCDEFGH"}, "abcdefgh"},
		{"with prefix", []string{"code: aabb2233"}, "aabb2233"},
		{"embedded in sentence", []string{"hello abcdefgh world"}, "abcdefgh"},
		{"second field used when first empty", []string{"", "from john abcdefgh"}, "abcdefgh"},
		{"first 8-char token wins", []string{"xyzpqrst aabbccdd"}, "xyzpqrst"},
		{"first field wins over second", []string{"abcdefgh", "ttttuuuu"}, "abcdefgh"},
		{"no match", []string{"no match here"}, ""},
		{"too short", []string{"abcdefg"}, ""},
		{"too long is rejected", []string{"abcdefghi"}, ""},
		{"digits 0/1/8/9 split the run", []string{"1abc2def"}, ""},
		{"all empty", []string{"", ""}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractCode(c.fields...)
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

// fakeStore is an in-memory implementation of Store for processor tests.
// One known order keyed by `code`; lookups for any other code return
// ErrCodeNotFound. ConfirmOrder flips the order to confirmed so a second
// processTx sees the already-paid branch.
type fakeStore struct {
	mu    sync.Mutex
	order Order
	items []OrderItem
	code  string

	failFind    error
	failConfirm error

	confirmCalls []confirmCall
}

type confirmCall struct {
	orderID    int64
	qrPayloads map[int64]string
}

func (f *fakeStore) FindOrderByCode(_ context.Context, code string) (Order, []OrderItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFind != nil {
		return Order{}, nil, f.failFind
	}
	if code != f.code {
		return Order{}, nil, ErrCodeNotFound
	}
	return f.order, f.items, nil
}

func (f *fakeStore) ConfirmOrder(_ context.Context, orderID int64, qrPayloads map[int64]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.confirmCalls = append(f.confirmCalls, confirmCall{orderID, qrPayloads})
	if f.failConfirm != nil {
		return f.failConfirm
	}
	now := time.Now()
	f.order.ConfirmedAt = &now
	return nil
}

type fakeCoder struct{}

func (fakeCoder) QRPayload(reservationID, seatID int64) string {
	return "qr-stub"
}

type fakeNotifier struct {
	mu    sync.Mutex
	sends []sendArgs
	fail  error
}

type sendArgs struct {
	chatID int64
	seat   Seat
	pdfLen int
}

func (f *fakeNotifier) SendTicket(chatID int64, seat Seat, pdf []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.sends = append(f.sends, sendArgs{chatID, seat, len(pdf)})
	return nil
}

func newTestProcessor(store *fakeStore, notifier *fakeNotifier) *Processor {
	return &Processor{
		Store:    store,
		Coder:    fakeCoder{},
		Notifier: notifier,
		Renderer: func(Show, Seat, string, string) ([]byte, error) { return []byte("PDF"), nil },
		ShowFn:   func(context.Context) (Show, error) { return Show{Title: "Test"}, nil },
	}
}

// fakeEmail captures every SendTicketBatchEmail call for assertion.
type fakeEmail struct {
	mu        sync.Mutex
	batches   [][]EmailItem
	cancels   []cancelCall
	err       error
	cancelErr error
}

type cancelCall struct {
	to        string
	buyerName string
	seat      Seat
}

func (f *fakeEmail) SendTicketBatchEmail(_ context.Context, _ string, _ string, items []EmailItem, _ Show) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	copyItems := make([]EmailItem, len(items))
	copy(copyItems, items)
	f.batches = append(f.batches, copyItems)
	return nil
}

func (f *fakeEmail) SendCancellationEmail(_ context.Context, to, name string, seat Seat, _ Show) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelErr != nil {
		return f.cancelErr
	}
	f.cancels = append(f.cancels, cancelCall{to, name, seat})
	return nil
}

func newTx(comment string, amount int64) bank.Transaction {
	return bank.Transaction{
		ID:      "tx-1",
		Comment: comment,
		Amount:  money.New(amount, currency.UAH),
	}
}

// --- test fixtures ---

func singleSeatOrder(code string, chatID int64, email string) (Order, []OrderItem) {
	order := Order{
		ID: 7, Code: code,
		BuyerName: "Анна", BuyerEmail: email, TGChatID: chatID,
		TotalKopecks: 25000,
	}
	items := []OrderItem{
		{ReservationID: 100, Seat: Seat{ID: 3, Row: 1, Col: 2, Price: money.New(25000, currency.UAH)}},
	}
	return order, items
}

func multiSeatOrder(code string, chatID int64, email string, n int) (Order, []OrderItem) {
	items := make([]OrderItem, n)
	var total int64
	for i := range n {
		items[i] = OrderItem{
			ReservationID: int64(100 + i),
			Seat:          Seat{ID: int64(10 + i), Row: 1, Col: i + 1, Price: money.New(25000, currency.UAH)},
		}
		total += 25000
	}
	order := Order{
		ID: 8, Code: code,
		BuyerName: "Мульти", BuyerEmail: email, TGChatID: chatID,
		TotalKopecks: total,
	}
	return order, items
}

// --- tests ---

func TestProcessor_HappyPath_SingleSeat(t *testing.T) {
	order, items := singleSeatOrder("abcdefgh", 42, "")
	st := &fakeStore{code: "abcdefgh", order: order, items: items}
	notifier := &fakeNotifier{}
	p := newTestProcessor(st, notifier)

	if err := p.Handle(context.Background(), &webhook.Response{
		Data: webhook.Data{Transaction: newTx("payment abcdefgh", 25000)},
	}); err != nil {
		t.Fatal(err)
	}
	if len(st.confirmCalls) != 1 {
		t.Errorf("confirm calls = %d, want 1", len(st.confirmCalls))
	}
	if len(notifier.sends) != 1 {
		t.Errorf("notifier sends = %d, want 1", len(notifier.sends))
	}
}

func TestProcessor_HappyPath_MultiSeat(t *testing.T) {
	order, items := multiSeatOrder("multi237", 42, "", 3)
	st := &fakeStore{code: "multi237", order: order, items: items}
	notifier := &fakeNotifier{}
	p := newTestProcessor(st, notifier)

	if err := p.Handle(context.Background(), &webhook.Response{
		Data: webhook.Data{Transaction: newTx("payment multi237", 75000)},
	}); err != nil {
		t.Fatal(err)
	}
	// One confirm for the order, N notifier sends — one PDF per seat.
	if len(st.confirmCalls) != 1 {
		t.Errorf("confirm calls = %d, want 1", len(st.confirmCalls))
	}
	if got := len(st.confirmCalls[0].qrPayloads); got != 3 {
		t.Errorf("qrs in confirm = %d, want 3", got)
	}
	if len(notifier.sends) != 3 {
		t.Errorf("notifier sends = %d, want 3 (one per seat)", len(notifier.sends))
	}
}

func TestProcessor_ShortAmountRejected(t *testing.T) {
	order, items := singleSeatOrder("abcdefgh", 42, "")
	st := &fakeStore{code: "abcdefgh", order: order, items: items}
	notifier := &fakeNotifier{}
	p := newTestProcessor(st, notifier)

	if err := p.Handle(context.Background(), &webhook.Response{
		Data: webhook.Data{Transaction: newTx("payment abcdefgh", 10000)},
	}); err != nil {
		t.Fatal(err)
	}
	if len(st.confirmCalls) != 0 {
		t.Errorf("Confirm called %d times on short payment", len(st.confirmCalls))
	}
	if len(notifier.sends) != 0 {
		t.Errorf("ticket sent on short payment")
	}
}

func TestProcessor_OverpaymentAccepted(t *testing.T) {
	order, items := singleSeatOrder("abcdefgh", 42, "")
	st := &fakeStore{code: "abcdefgh", order: order, items: items}
	notifier := &fakeNotifier{}
	p := newTestProcessor(st, notifier)

	if err := p.Handle(context.Background(), &webhook.Response{
		Data: webhook.Data{Transaction: newTx("payment abcdefgh", 50000)},
	}); err != nil {
		t.Fatal(err)
	}
	if len(st.confirmCalls) != 1 {
		t.Errorf("confirm calls = %d, want 1", len(st.confirmCalls))
	}
}

func TestProcessor_WebhookReplayDoesNotDoubleIssue(t *testing.T) {
	order, items := singleSeatOrder("replaycd", 42, "")
	st := &fakeStore{code: "replaycd", order: order, items: items}
	notifier := &fakeNotifier{}
	p := newTestProcessor(st, notifier)

	// First delivery confirms.
	if err := p.Handle(context.Background(), &webhook.Response{
		Data: webhook.Data{Transaction: newTx("payment replaycd", 25000)},
	}); err != nil {
		t.Fatal(err)
	}
	// Second delivery sees order.ConfirmedAt != nil, short-circuits.
	if err := p.Handle(context.Background(), &webhook.Response{
		Data: webhook.Data{Transaction: newTx("payment replaycd", 25000)},
	}); err != nil {
		t.Fatal(err)
	}
	if len(notifier.sends) != 1 {
		t.Errorf("notifier sends = %d, want 1 (idempotent)", len(notifier.sends))
	}
}

func TestProcessor_WebBuyerGetsEmailNoTelegram(t *testing.T) {
	order, items := singleSeatOrder("emailcde", 0, "buyer@example.com")
	st := &fakeStore{code: "emailcde", order: order, items: items}
	notifier := &fakeNotifier{}
	mailer := &fakeEmail{}
	p := newTestProcessor(st, notifier)
	p.Email = mailer

	if err := p.Handle(context.Background(), &webhook.Response{
		Data: webhook.Data{Transaction: newTx("payment emailcde", 25000)},
	}); err != nil {
		t.Fatal(err)
	}
	if len(notifier.sends) != 0 {
		t.Errorf("Telegram should NOT fire for web buyer (chatID=0), got %d", len(notifier.sends))
	}
	if len(mailer.batches) != 1 || len(mailer.batches[0]) != 1 {
		t.Errorf("email batches = %v, want one with one item", mailer.batches)
	}
}

func TestProcessor_MultiSeatEmailBatch(t *testing.T) {
	order, items := multiSeatOrder("multim7r", 0, "buyer@x.com", 3)
	st := &fakeStore{code: "multim7r", order: order, items: items}
	mailer := &fakeEmail{}
	notifier := &fakeNotifier{}
	p := newTestProcessor(st, notifier)
	p.Email = mailer

	if err := p.Handle(context.Background(), &webhook.Response{
		Data: webhook.Data{Transaction: newTx("payment multim7r", 75000)},
	}); err != nil {
		t.Fatal(err)
	}
	// One email with three attachments — not three emails.
	if len(mailer.batches) != 1 {
		t.Fatalf("emails sent = %d, want 1 batched message", len(mailer.batches))
	}
	if got := len(mailer.batches[0]); got != 3 {
		t.Errorf("attachments in batch = %d, want 3", got)
	}
}

func TestProcessor_EmailFailureDoesNotBlockConfirm(t *testing.T) {
	order, items := singleSeatOrder("failmail", 0, "buyer@x.com")
	st := &fakeStore{code: "failmail", order: order, items: items}
	notifier := &fakeNotifier{}
	mailer := &fakeEmail{err: errors.New("smtp down")}
	p := newTestProcessor(st, notifier)
	p.Email = mailer

	if err := p.Handle(context.Background(), &webhook.Response{
		Data: webhook.Data{Transaction: newTx("payment failmail", 25000)},
	}); err != nil {
		t.Fatalf("Handle should swallow SMTP failure: %v", err)
	}
	if len(st.confirmCalls) != 1 {
		t.Errorf("confirm calls = %d, want 1 (email failure must not block confirm)", len(st.confirmCalls))
	}
}

func TestProcessor_NoCodeInCommentIgnored(t *testing.T) {
	st := &fakeStore{code: "abcdefgh"}
	notifier := &fakeNotifier{}
	p := newTestProcessor(st, notifier)
	if err := p.Handle(context.Background(), &webhook.Response{
		Data: webhook.Data{Transaction: newTx("just a transfer", 25000)},
	}); err != nil {
		t.Fatal(err)
	}
	if len(st.confirmCalls) != 0 {
		t.Errorf("Confirm called for transaction without code")
	}
}

func TestProcessor_ReconcileTxAfterWebhookIsNoOp(t *testing.T) {
	order, items := singleSeatOrder("dblpay76", 42, "")
	st := &fakeStore{code: "dblpay76", order: order, items: items}
	notifier := &fakeNotifier{}
	p := newTestProcessor(st, notifier)

	tx := newTx("payment dblpay76", 25000)
	if err := p.Handle(context.Background(), &webhook.Response{
		Data: webhook.Data{Transaction: tx},
	}); err != nil {
		t.Fatal(err)
	}
	matched, err := p.ReconcileTx(context.Background(), tx)
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Errorf("ReconcileTx after webhook returned matched=true, want false")
	}
	if len(notifier.sends) != 1 {
		t.Errorf("notifier sends = %d, want 1 (no duplicate ticket)", len(notifier.sends))
	}
}
