// Package bot is the Telegram side of the ticket app: seat picking,
// payment instructions, cancellation, /my, /stats.
package bot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/OlexiyOdarchuk/go-monobank-sdk/money"
	tele "gopkg.in/telebot.v3"
)

// Show is the subset of show info the bot needs.
type Show struct {
	ID       int64
	Title    string
	Venue    string
	StartsAt time.Time
}

// Seat is the subset of seat info the bot needs.
type Seat struct {
	ID           int64
	ShowID       int64
	Row, Col     int
	PriceKopecks int64
}

// Reservation mirrors a stored reservation row.
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

// MyItem couples a reservation with its seat for /my output.
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

// SeatStatus is one of "free", "held", "sold".
type SeatStatus string

const (
	SeatFree SeatStatus = "free"
	SeatHeld SeatStatus = "held"
	SeatSold SeatStatus = "sold"
)

// Domain errors the bot expects the Store to return.
var (
	ErrSeatTaken      = errors.New("seat is already reserved or sold")
	ErrSeatNotFound   = errors.New("seat does not exist for this show")
	ErrCodeNotFound   = errors.New("reservation code not found")
	ErrAlreadyPaid    = errors.New("reservation already confirmed")
	ErrAlreadyClosed  = errors.New("reservation already closed")
	ErrNotYourBooking = errors.New("reservation belongs to another user")
)

// Store is the persistence behavior the bot needs.
type Store interface {
	Seats(ctx context.Context, showID int64) ([]Seat, error)
	SeatStatuses(ctx context.Context, showID int64) (map[int64]SeatStatus, error)
	FindFreeSeat(ctx context.Context, showID int64, row, col int) (Seat, error)
	Reserve(ctx context.Context, seat Seat, tgUserID, tgChatID int64, buyerName, code string, hold time.Duration) (Reservation, error)
	CancelReservation(ctx context.Context, code string, tgUserID int64) (Reservation, Seat, error)
	MyReservations(ctx context.Context, tgUserID int64) ([]MyItem, error)
	Stats(ctx context.Context, showID int64) (Stats, error)
}

// Coder issues short reservation codes.
type Coder interface {
	NewCode() (string, error)
}

// ReconcileResult is the outcome of a /reconcile sweep.
type ReconcileResult struct {
	Scanned int // transactions examined
	Matched int // tickets newly issued
}

// Reconciler scans recent statement entries and confirms any whose
// reservation code matches an open booking — a rescue net for webhook
// events Mono never delivered. The optional progress callback is invoked
// between accounts so the bot can keep the admin informed during the
// minute-long monobank rate-limit waits.
type Reconciler interface {
	Reconcile(ctx context.Context, lookback time.Duration, progress func(string)) (ReconcileResult, error)
}

// JarBalance is the snapshot returned by JarLookup.
type JarBalance struct {
	Title   string
	Owner   string
	Balance money.Money
	Goal    money.Money // zero if no goal set
}

// JarLookup returns the live balance of the configured monobank jar.
type JarLookup interface {
	Balance(ctx context.Context) (JarBalance, error)
}

type Bot struct {
	tb         *tele.Bot
	store      Store
	coder      Coder
	show       Show
	jarLink    string
	hold       time.Duration
	adminTGID  int64
	reconciler Reconciler // optional — nil if MONO_TOKEN missing
	jar        JarLookup  // optional — nil if jar link unparseable

	// pending tracks users who clicked a seat and are about to type their
	// name. Keyed by Telegram user ID; value is pendingPick. A background
	// sweeper drops entries past Until so abandoned picks don't accumulate
	// for the lifetime of the process.
	pending sync.Map
	done    chan struct{}
}

type pendingPick struct {
	Row, Col int
	Until    time.Time
}

type Options struct {
	Token      string
	Store      Store
	Coder      Coder
	Show       Show
	JarLink    string
	Hold       time.Duration
	AdminTGID  int64
	Reconciler Reconciler // optional
	Jar        JarLookup  // optional
}

func New(opts Options) (*Bot, error) {
	tb, err := tele.NewBot(tele.Settings{
		Token:  opts.Token,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	})
	if err != nil {
		return nil, err
	}
	b := &Bot{
		tb: tb, store: opts.Store, coder: opts.Coder, show: opts.Show,
		jarLink: opts.JarLink, hold: opts.Hold, adminTGID: opts.AdminTGID,
		reconciler: opts.Reconciler, jar: opts.Jar,
		done: make(chan struct{}),
	}
	b.routes()
	go b.sweepPending(time.Minute)
	return b, nil
}

func (b *Bot) Start() { b.tb.Start() }

func (b *Bot) Stop() {
	select {
	case <-b.done:
	default:
		close(b.done)
	}
	b.tb.Stop()
}

// sweepPending drops pending picks whose TTL has lapsed. Without this an
// abandoned pick lives for the lifetime of the process — small, but real.
func (b *Bot) sweepPending(every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-b.done:
			return
		case now := <-tick.C:
			b.pending.Range(func(k, v any) bool {
				if p, ok := v.(pendingPick); ok && now.After(p.Until) {
					b.pending.Delete(k)
				}
				return true
			})
		}
	}
}

// SetReconciler wires the rescue-net scanner. Pass nil to disable.
// Used when the reconciler depends on something built after the bot
// (e.g., a pay.Processor that itself needs the bot as a notifier).
func (b *Bot) SetReconciler(r Reconciler) { b.reconciler = r }

// SetJar wires the jar-balance lookup. Pass nil to disable.
func (b *Bot) SetJar(j JarLookup) { b.jar = j }

// SendTicket pushes a generated PDF back to the buyer's chat.
func (b *Bot) SendTicket(chatID int64, seat Seat, pdf []byte) error {
	doc := &tele.Document{
		File:     tele.FromReader(bytes.NewReader(pdf)),
		FileName: fmt.Sprintf("ticket-%d-%d.pdf", seat.Row, seat.Col),
		Caption:  fmt.Sprintf("Готово ✅\nРяд %d, місце %d", seat.Row, seat.Col),
	}
	_, err := b.tb.Send(tele.ChatID(chatID), doc)
	return err
}

// NotifyShowSoon pings a buyer about the upcoming show.
func (b *Bot) NotifyShowSoon(chatID int64, seat Seat, when time.Time) error {
	_, err := b.tb.Send(tele.ChatID(chatID), fmt.Sprintf(
		"Привіт! Нагадую: %s — вже сьогодні о %s.\nТвоє місце: ряд %d · %d.\nЧекаємо!",
		b.show.Title, formatClock(when), seat.Row, seat.Col))
	return err
}

func (b *Bot) routes() {
	b.tb.Handle("/start", b.handleStart)
	b.tb.Handle("/seats", b.handleSeats)
	b.tb.Handle("/my", b.handleMy)
	b.tb.Handle("/stats", b.handleStats)
	b.tb.Handle("/reconcile", b.handleReconcile)
	b.tb.Handle("/jar", b.handleJar)
	b.tb.Handle(tele.OnText, b.handleText)
	b.tb.Handle(tele.OnCallback, b.handleCallback)
}

func (b *Bot) handleStart(c tele.Context) error {
	cmds := "  /seats — мапа місць\n  /my — мої бронювання\n"
	if b.adminTGID != 0 && c.Sender().ID == b.adminTGID {
		cmds += "  /stats — статистика (адмін)\n  /reconcile — звірити пропущені оплати (адмін)\n  /jar — баланс банки (адмін)\n"
	}
	return c.Send(fmt.Sprintf(
		"Вітаю! Це бот продажу квитків на %q (%s, %s).\n\n"+
			"Команди:\n%s\n"+
			"Щоб купити: натисни /seats → обери вільне місце.",
		b.show.Title, b.show.Venue, formatDateTime(b.show.StartsAt), cmds))
}

func (b *Bot) handleSeats(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	seats, err := b.store.Seats(ctx, b.show.ID)
	if err != nil {
		return c.Send("storage error: " + err.Error())
	}
	status, err := b.store.SeatStatuses(ctx, b.show.ID)
	if err != nil {
		return c.Send("storage error: " + err.Error())
	}

	rows := make(map[int][]Seat)
	maxRow := 0
	for _, s := range seats {
		rows[s.Row] = append(rows[s.Row], s)
		if s.Row > maxRow {
			maxRow = s.Row
		}
	}

	markup := &tele.ReplyMarkup{}
	keyboard := make([][]tele.InlineButton, 0, maxRow)
	for r := 1; r <= maxRow; r++ {
		kbRow := make([]tele.InlineButton, 0, len(rows[r]))
		for _, s := range rows[r] {
			label := fmt.Sprintf("%d", s.Col)
			switch status[s.ID] {
			case SeatSold:
				label = "✖"
			case SeatHeld:
				label = "…"
			}
			kbRow = append(kbRow, tele.InlineButton{
				Unique: "seat",
				Text:   label,
				Data:   fmt.Sprintf("%d:%d", s.Row, s.Col),
			})
		}
		keyboard = append(keyboard, kbRow)
	}
	markup.InlineKeyboard = keyboard

	header := "🎭 ━━━━━ СЦЕНА ━━━━━ 🎭\n               ▲ попереду\n\n"
	return c.Send(fmt.Sprintf("%sРяд 1 — найближче до сцени.\nНатисни вільне місце, щоб забронювати.\nЦіна: %s",
		header, hryvnia(seats[0].PriceKopecks)), markup)
}

func (b *Bot) handleCallback(c tele.Context) error {
	cb := c.Callback()
	if cb == nil {
		return nil
	}
	switch {
	case strings.HasPrefix(cb.Data, "\fseat|"):
		return b.callbackSeat(c, cb)
	case strings.HasPrefix(cb.Data, "\fcancel|"):
		return b.callbackCancel(c, cb)
	}
	return c.Respond(&tele.CallbackResponse{Text: "unknown action"})
}

// pendingTTL bounds how long a user has to type their name after picking a
// seat. Long enough for someone to switch apps and come back; short enough
// that an abandoned pick doesn't reserve mind-share.
const pendingTTL = 10 * time.Minute

func (b *Bot) callbackSeat(c tele.Context, cb *tele.Callback) error {
	data := strings.TrimPrefix(cb.Data, "\fseat|")
	parts := strings.SplitN(data, ":", 2)
	if len(parts) != 2 {
		return c.Respond(&tele.CallbackResponse{Text: "bad seat"})
	}
	row, err1 := strconv.Atoi(parts[0])
	col, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return c.Respond(&tele.CallbackResponse{Text: "bad seat"})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Pre-check the seat is free so we don't ask for a name on a taken seat.
	// We re-check on Reserve to close the (small) race window.
	seat, err := b.store.FindFreeSeat(ctx, b.show.ID, row, col)
	if err != nil {
		return c.Respond(&tele.CallbackResponse{Text: friendly(err)})
	}

	b.pending.Store(cb.Sender.ID, pendingPick{
		Row: row, Col: col,
		Until: time.Now().Add(pendingTTL),
	})
	_ = c.Respond(&tele.CallbackResponse{Text: "Введи ім'я"})

	_, err = b.tb.Send(cb.Sender, fmt.Sprintf(
		"Місце ряд %d · %d вибрано.\n"+
			"Введи ім'я та прізвище — вони будуть надруковані на квитку:",
		seat.Row, seat.Col),
		&tele.ReplyMarkup{ForceReply: true, Selective: true})
	return err
}

func (b *Bot) callbackCancel(c tele.Context, cb *tele.Callback) error {
	code := strings.TrimPrefix(cb.Data, "\fcancel|")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, seat, err := b.store.CancelReservation(ctx, code, cb.Sender.ID)
	if err != nil {
		switch {
		case errors.Is(err, ErrCodeNotFound), errors.Is(err, ErrAlreadyClosed):
			return c.Respond(&tele.CallbackResponse{Text: "Цю бронь вже закрито"})
		case errors.Is(err, ErrAlreadyPaid):
			return c.Respond(&tele.CallbackResponse{Text: "Вже оплачено — повернення тільки руками"})
		case errors.Is(err, ErrNotYourBooking):
			return c.Respond(&tele.CallbackResponse{Text: "Це не твоя бронь"})
		default:
			return c.Respond(&tele.CallbackResponse{Text: "Помилка"})
		}
	}
	_ = c.Respond(&tele.CallbackResponse{Text: "Скасовано"})
	_, err = b.tb.Send(cb.Sender,
		fmt.Sprintf("Бронь ряд %d місце %d скасовано. Місце знов вільне.", seat.Row, seat.Col))
	return err
}

func (b *Bot) handleMy(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	items, err := b.store.MyReservations(ctx, c.Sender().ID)
	if err != nil {
		return c.Send("storage error: " + err.Error())
	}
	if len(items) == 0 {
		return c.Send("У тебе ще немає бронювань. /seats — мапа місць.")
	}

	var out strings.Builder
	out.WriteString("Твої бронювання:\n\n")
	for _, it := range items {
		switch {
		case it.Reservation.ConfirmedAt != nil:
			fmt.Fprintf(&out, "✅ Ряд %d місце %d — оплачено (%s)\n",
				it.Seat.Row, it.Seat.Col, formatDateTime(*it.Reservation.ConfirmedAt))
		case it.Reservation.ExpiresAt.After(time.Now()):
			fmt.Fprintf(&out, "⏳ Ряд %d місце %d — чекає оплати до %s\n   код: `%s`\n",
				it.Seat.Row, it.Seat.Col, formatClock(it.Reservation.ExpiresAt), it.Reservation.Code)
		default:
			fmt.Fprintf(&out, "✖ Ряд %d місце %d — бронь протермінувалась\n", it.Seat.Row, it.Seat.Col)
		}
	}
	return c.Send(out.String(), tele.ModeMarkdown)
}

func (b *Bot) handleStats(c tele.Context) error {
	if b.adminTGID == 0 || c.Sender().ID != b.adminTGID {
		return c.Send("⛔️ команда тільки для адміна")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := b.store.Stats(ctx, b.show.ID)
	if err != nil {
		return c.Send("storage error: " + err.Error())
	}
	return c.Send(fmt.Sprintf(
		"📊 *%s*\n"+
			"%s · %s\n\n"+
			"всього місць: *%d*\n"+
			"продано: *%d*\n"+
			"в очікуванні оплати: *%d*\n"+
			"вільно: *%d*\n\n"+
			"виторг: *%s*",
		b.show.Title, b.show.Venue, formatDateTime(b.show.StartsAt),
		st.Total, st.Sold, st.Held, st.Free, hryvnia(st.RevenueKopecks)), tele.ModeMarkdown)
}

func (b *Bot) handleReconcile(c tele.Context) error {
	if b.adminTGID == 0 || c.Sender().ID != b.adminTGID {
		return c.Send("⛔️ команда тільки для адміна")
	}
	if b.reconciler == nil {
		return c.Send("Реконсайл недоступний: MONO_TOKEN не налаштований.")
	}
	// Default lookback: 24h. "/reconcile 7d" — explicit window.
	lookback := 24 * time.Hour
	if args := strings.TrimSpace(c.Message().Payload); args != "" {
		d, err := time.ParseDuration(args)
		if err != nil || d <= 0 {
			return c.Send("Формат: /reconcile [тривалість], напр. `/reconcile 48h`", tele.ModeMarkdown)
		}
		if d > 30*24*time.Hour {
			return c.Send("Максимум — 720h (30 днів). Mono не віддає старіше.")
		}
		lookback = d
	}
	progressMsg, err := b.tb.Send(c.Chat(),
		fmt.Sprintf("⏳ Шукаю пропущені оплати за останні %s…", lookback))
	if err != nil {
		// If we can't even send the ack, do the work anyway and hope the
		// final reply lands.
		log.Printf("reconcile ack: %v", err)
	}
	// Live-edit the progress message instead of spamming. Mono is rate-
	// limited to 1 request/60s per account, so updates fire at most once
	// a minute — well within Telegram's edit budget.
	progress := func(s string) {
		if progressMsg == nil {
			return
		}
		if _, err := b.tb.Edit(progressMsg, "⏳ "+s); err != nil {
			log.Printf("reconcile progress edit: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res, err := b.reconciler.Reconcile(ctx, lookback, progress)
	if err != nil {
		return c.Send("Помилка: " + err.Error())
	}
	return c.Send(fmt.Sprintf("✅ Готово.\nПереглянуто транзакцій: *%d*\nВидано квитків: *%d*",
		res.Scanned, res.Matched), tele.ModeMarkdown)
}

func (b *Bot) handleJar(c tele.Context) error {
	if b.adminTGID == 0 || c.Sender().ID != b.adminTGID {
		return c.Send("⛔️ команда тільки для адміна")
	}
	if b.jar == nil {
		return c.Send("Інформація про банку недоступна (не вдалося розпарсити MONO_JAR_LINK).")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	info, err := b.jar.Balance(ctx)
	if err != nil {
		return c.Send("Помилка: " + err.Error())
	}
	out := fmt.Sprintf("🏦 *%s*\n", info.Title)
	if info.Owner != "" {
		out += "власник: " + info.Owner + "\n"
	}
	out += "зібрано: *" + info.Balance.String() + "*"
	if !info.Goal.IsZero() {
		out += "\nціль: " + info.Goal.String()
	}
	return c.Send(out, tele.ModeMarkdown)
}

func (b *Bot) handleText(c tele.Context) error {
	sender := c.Sender()
	if sender == nil {
		return nil
	}
	raw, ok := b.pending.Load(sender.ID)
	if !ok {
		return nil // user typed text but isn't in name-input mode
	}
	pick, ok := raw.(pendingPick)
	if !ok {
		b.pending.Delete(sender.ID)
		return nil
	}
	if time.Now().After(pick.Until) {
		b.pending.Delete(sender.ID)
		return c.Send("Час очікування імені вийшов. Натисни /seats і вибери місце ще раз.")
	}

	name, err := normalizeName(c.Text())
	if err != nil {
		return c.Send(err.Error() + "\nСпробуй ще раз:")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	seat, err := b.store.FindFreeSeat(ctx, b.show.ID, pick.Row, pick.Col)
	if err != nil {
		b.pending.Delete(sender.ID)
		return c.Send(friendly(err))
	}
	code, err := b.coder.NewCode()
	if err != nil {
		return c.Send("Внутрішня помилка, спробуй пізніше")
	}
	r, err := b.store.Reserve(ctx, seat, sender.ID, c.Chat().ID, name, code, b.hold)
	if err != nil {
		b.pending.Delete(sender.ID)
		return c.Send(friendly(err))
	}
	b.pending.Delete(sender.ID)

	payURL := jarPrefillURL(b.jarLink, seat.PriceKopecks, r.Code)
	payBtn := tele.InlineButton{Text: "💳 Оплатити", URL: payURL}
	cancelBtn := tele.InlineButton{Unique: "cancel", Text: "✖ Скасувати бронь", Data: r.Code}
	// Pay on top, cancel below — reach-for-the-first-button habit lands on
	// the safe action, not the destructive one.
	markup := &tele.ReplyMarkup{InlineKeyboard: [][]tele.InlineButton{{payBtn}, {cancelBtn}}}

	return c.Send(fmt.Sprintf(
		"Місце забронювано: ряд %d, місце %d.\n"+
			"На квитку буде: *%s*\n\n"+
			"💳 Натисни *Оплатити* — сума й коментар вже вписані.\n"+
			"Код у коментарі — `%s`.\n"+
			"Бронювання дійсне до %s. Після оплати бот сам пришле PDF.",
		seat.Row, seat.Col, name, r.Code, formatClock(r.ExpiresAt)),
		tele.ModeMarkdown,
		&tele.SendOptions{DisableWebPagePreview: true},
		markup)
}

// nameMaxRunes caps the buyer name at a length that fits one line of the
// A6 PDF (105mm wide) at font size 11. Names longer than this either wrap
// or visually crowd the seat callout below.
const nameMaxRunes = 60

// normalizeName trims, collapses spaces and enforces a 2–nameMaxRunes rune
// length. Letters, digits, spaces, apostrophes, hyphens and dots are
// accepted — enough for Ukrainian double-barrelled names like
// "Анна-Марія О'Брайен".
func normalizeName(in string) (string, error) {
	n := strings.Join(strings.Fields(in), " ")
	rc := utf8.RuneCountInString(n)
	if rc < 2 {
		return "", fmt.Errorf("ім'я має бути щонайменше 2 символи")
	}
	if rc > nameMaxRunes {
		return "", fmt.Errorf("ім'я задовге (макс. %d символів)", nameMaxRunes)
	}
	return n, nil
}

// jarPrefillURL appends ?a=<amount>&t=<comment>. Short keys are what mono's
// jar page actually honours in practice.
func jarPrefillURL(base string, kopecks int64, comment string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	var amount string
	if kopecks%100 == 0 {
		amount = fmt.Sprintf("%d", kopecks/100)
	} else {
		amount = fmt.Sprintf("%d.%02d", kopecks/100, kopecks%100)
	}
	q := u.Query()
	q.Set("a", amount)
	q.Set("t", comment)
	u.RawQuery = q.Encode()
	return u.String()
}

func friendly(err error) string {
	switch {
	case errors.Is(err, ErrSeatTaken):
		return "Це місце вже зайняте"
	case errors.Is(err, ErrSeatNotFound):
		return "Такого місця нема"
	default:
		return "Помилка"
	}
}

func hryvnia(kopecks int64) string {
	return fmt.Sprintf("%d.%02d UAH", kopecks/100, kopecks%100)
}

var ukMonthsGenitive = [...]string{
	"січня", "лютого", "березня", "квітня", "травня", "червня",
	"липня", "серпня", "вересня", "жовтня", "листопада", "грудня",
}

func formatDateTime(t time.Time) string {
	return fmt.Sprintf("%d %s %d · %s",
		t.Day(), ukMonthsGenitive[t.Month()-1], t.Year(), t.Format("15:04"))
}

func formatClock(t time.Time) string { return t.Format("15:04") }
