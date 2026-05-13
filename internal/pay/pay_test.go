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
type fakeStore struct {
	mu          sync.Mutex
	reservation Reservation
	seat        Seat
	codeKnown   string

	// failFind / failConfirm tag specific failure modes the test wants.
	failFind    error
	failConfirm error

	confirmCalls []confirmCall
}

type confirmCall struct {
	reservationID int64
	qrPayload     string
}

func (f *fakeStore) FindReservationByCode(_ context.Context, code string) (Reservation, Seat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFind != nil {
		return Reservation{}, Seat{}, f.failFind
	}
	if code != f.codeKnown {
		return Reservation{}, Seat{}, ErrCodeNotFound
	}
	return f.reservation, f.seat, nil
}

func (f *fakeStore) Confirm(_ context.Context, reservationID int64, qrPayload string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.confirmCalls = append(f.confirmCalls, confirmCall{reservationID, qrPayload})
	if f.failConfirm != nil {
		return f.failConfirm
	}
	// First successful confirm flips the in-memory reservation to confirmed,
	// so a second processTx() under the same code sees the "already paid"
	// branch and short-circuits.
	now := time.Now()
	f.reservation.ConfirmedAt = &now
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
		Show:     Show{Title: "Test"},
		MinPrice: money.New(25000, currency.UAH),
	}
}

func newTx(comment string, amount int64) bank.Transaction {
	return bank.Transaction{
		ID:      "tx-1",
		Comment: comment,
		Amount:  money.New(amount, currency.UAH),
	}
}

func TestProcessor_HappyPath(t *testing.T) {
	st := &fakeStore{
		codeKnown:   "abcdefgh",
		reservation: Reservation{ID: 7, TGChatID: 42, BuyerName: "Анна"},
		seat:        Seat{ID: 3, Row: 1, Col: 2, Price: money.New(25000, currency.UAH)},
	}
	notifier := &fakeNotifier{}
	p := newTestProcessor(st, notifier)

	err := p.Handle(context.Background(), &webhook.Response{
		Data: webhook.Data{Transaction: newTx("payment abcdefgh", 25000)},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(st.confirmCalls) != 1 {
		t.Fatalf("Confirm calls = %d, want 1", len(st.confirmCalls))
	}
	if len(notifier.sends) != 1 {
		t.Fatalf("notifier sends = %d, want 1", len(notifier.sends))
	}
	if notifier.sends[0].chatID != 42 {
		t.Errorf("chat = %d, want 42", notifier.sends[0].chatID)
	}
}

func TestProcessor_WebhookDeliveredTwiceIsIdempotent(t *testing.T) {
	st := &fakeStore{
		codeKnown:   "abcdefgh",
		reservation: Reservation{ID: 7, TGChatID: 42, BuyerName: "Анна"},
		seat:        Seat{ID: 3, Row: 1, Col: 2, Price: money.New(25000, currency.UAH)},
	}
	notifier := &fakeNotifier{}
	p := newTestProcessor(st, notifier)
	tx := newTx("payment abcdefgh", 25000)

	if err := p.Handle(context.Background(), &webhook.Response{Data: webhook.Data{Transaction: tx}}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := p.Handle(context.Background(), &webhook.Response{Data: webhook.Data{Transaction: tx}}); err != nil {
		t.Fatalf("second: %v", err)
	}
	// First call confirmed; second call sees ConfirmedAt != nil and short-circuits
	// before calling Confirm. So Confirm is called exactly once.
	if len(st.confirmCalls) != 1 {
		t.Fatalf("Confirm calls = %d, want 1 (idempotent)", len(st.confirmCalls))
	}
	if len(notifier.sends) != 1 {
		t.Fatalf("notifier sends = %d, want 1 (idempotent)", len(notifier.sends))
	}
}

func TestProcessor_ReconcileAfterWebhookIsIdempotent(t *testing.T) {
	st := &fakeStore{
		codeKnown:   "abcdefgh",
		reservation: Reservation{ID: 7, TGChatID: 42, BuyerName: "Анна"},
		seat:        Seat{ID: 3, Row: 1, Col: 2, Price: money.New(25000, currency.UAH)},
	}
	notifier := &fakeNotifier{}
	p := newTestProcessor(st, notifier)
	tx := newTx("payment abcdefgh", 25000)

	if err := p.Handle(context.Background(), &webhook.Response{Data: webhook.Data{Transaction: tx}}); err != nil {
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

func TestProcessor_ShortAmountRejected(t *testing.T) {
	st := &fakeStore{
		codeKnown:   "abcdefgh",
		reservation: Reservation{ID: 7, TGChatID: 42, BuyerName: "Анна"},
		seat:        Seat{ID: 3, Row: 1, Col: 2, Price: money.New(25000, currency.UAH)},
	}
	notifier := &fakeNotifier{}
	p := newTestProcessor(st, notifier)

	// Paid 100.00 UAH on a 250.00 UAH seat — rejected, no confirm, no PDF.
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
	st := &fakeStore{
		codeKnown:   "abcdefgh",
		reservation: Reservation{ID: 7, TGChatID: 42, BuyerName: "Анна"},
		seat:        Seat{ID: 3, Row: 1, Col: 2, Price: money.New(25000, currency.UAH)},
	}
	notifier := &fakeNotifier{}
	p := newTestProcessor(st, notifier)

	// Paid 300.00 UAH on a 250.00 UAH seat — overpay is fine.
	if err := p.Handle(context.Background(), &webhook.Response{
		Data: webhook.Data{Transaction: newTx("payment abcdefgh", 30000)},
	}); err != nil {
		t.Fatal(err)
	}
	if len(notifier.sends) != 1 {
		t.Errorf("overpayment was not honoured")
	}
}

func TestProcessor_CodeNotFoundIsNoOp(t *testing.T) {
	st := &fakeStore{codeKnown: "abcdefgh"}
	notifier := &fakeNotifier{}
	p := newTestProcessor(st, notifier)

	// Comment carries a code that doesn't map to any reservation.
	if err := p.Handle(context.Background(), &webhook.Response{
		Data: webhook.Data{Transaction: newTx("payment xxxxxxxx", 25000)},
	}); err != nil {
		t.Fatal(err)
	}
	if len(st.confirmCalls) != 0 || len(notifier.sends) != 0 {
		t.Errorf("unknown code should be a no-op (confirms=%d sends=%d)",
			len(st.confirmCalls), len(notifier.sends))
	}
}

func TestProcessor_NoCodeInCommentIsNoOp(t *testing.T) {
	st := &fakeStore{codeKnown: "abcdefgh"}
	notifier := &fakeNotifier{}
	p := newTestProcessor(st, notifier)

	if err := p.Handle(context.Background(), &webhook.Response{
		Data: webhook.Data{Transaction: newTx("just a comment, no code here", 25000)},
	}); err != nil {
		t.Fatal(err)
	}
	if len(st.confirmCalls) != 0 {
		t.Errorf("Confirm called when no code was present in the comment")
	}
}

func TestProcessor_OutflowIgnored(t *testing.T) {
	st := &fakeStore{codeKnown: "abcdefgh"}
	notifier := &fakeNotifier{}
	p := newTestProcessor(st, notifier)

	// Negative amount = outflow. Should be silently ignored.
	if err := p.Handle(context.Background(), &webhook.Response{
		Data: webhook.Data{Transaction: newTx("payment abcdefgh", -25000)},
	}); err != nil {
		t.Fatal(err)
	}
	if len(st.confirmCalls) != 0 {
		t.Errorf("Confirm called on outflow")
	}
}

func TestProcessor_AlreadyClosedReservationIsNoOp(t *testing.T) {
	st := &fakeStore{
		codeKnown: "abcdefgh",
		failFind:  ErrAlreadyClosed,
	}
	notifier := &fakeNotifier{}
	p := newTestProcessor(st, notifier)

	if err := p.Handle(context.Background(), &webhook.Response{
		Data: webhook.Data{Transaction: newTx("payment abcdefgh", 25000)},
	}); err != nil {
		t.Fatal(err)
	}
	if len(st.confirmCalls) != 0 || len(notifier.sends) != 0 {
		t.Errorf("cancelled reservation should be a no-op")
	}
}

func TestProcessor_StorePropagatesUnknownErrors(t *testing.T) {
	boom := errors.New("db is on fire")
	st := &fakeStore{
		codeKnown:   "abcdefgh",
		reservation: Reservation{ID: 7, TGChatID: 42, BuyerName: "Анна"},
		seat:        Seat{ID: 3, Row: 1, Col: 2, Price: money.New(25000, currency.UAH)},
		failConfirm: boom,
	}
	notifier := &fakeNotifier{}
	p := newTestProcessor(st, notifier)

	err := p.Handle(context.Background(), &webhook.Response{
		Data: webhook.Data{Transaction: newTx("payment abcdefgh", 25000)},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got err=%v, want propagated %q", err, boom)
	}
	if len(notifier.sends) != 0 {
		t.Errorf("ticket was sent despite Confirm failing")
	}
}
