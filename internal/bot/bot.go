// Package bot is the Telegram side of the ticket app.
//
// UX model:
//   - /start без deep-link → афіша подій як inline keyboard.
//   - Тап події → меню з кнопками "📋 Обрати місце" (in-chat multi-pick),
//     "🗺 Відкрити мапу залу" (Telegram WebApp → /event/<slug>, лише
//     якщо заданий BASE_URL) і "↩ До списку".
//   - In-chat picker: тап місця додає його в кошик (✓ маркер), тап ще
//     раз — прибирає. Footer "✅ Завершити (N · сума)" і "🧹 Очистити".
//     Після Завершити — питає ім'я (ForceReply), створює один order на
//     всі обрані місця.
//   - /start res_<code> deep link з public web-flow: прив'язує цей чат
//     до order'а, щоб PDF'и прийшли сюди після оплати.
package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/OlexiyOdarchuk/go-monobank-sdk/money"
	tele "gopkg.in/telebot.v3"

	"github.com/OlexiyOdarchuk/monokasa/internal/realtime"
)

// Show is the subset of show info the bot needs. Slug is used to build
// the WebApp / public-link URLs the bot ships to buyers; Description is
// included in the show menu card.
type Show struct {
	ID          int64
	Slug        string
	Title       string
	Venue       string
	StartsAt    time.Time
	Description string
}

// Seat is the subset of seat info the bot needs (reservation cards,
// reminders, ticket captions, and the inline-keyboard picker).
type Seat struct {
	ID       int64
	ShowID   int64
	Row, Col int
	Price    money.Money
	Sellable bool
}

// Order is the bot-side view of a payment-grouping order. Used by
// the /start res_<code> deep link to attach a chat to a web-buyer's
// pending order so the PDFs land in Telegram on payment.
type Order struct {
	ID          int64
	Code        string
	BuyerName   string
	BuyerEmail  string
	TGChatID    int64
	ConfirmedAt *time.Time
}

// OrderItem couples a reservation with its seat for /my output and
// linking confirmations.
type OrderItem struct {
	Reservation Reservation
	Seat        Seat
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

// Stats is an admin snapshot of one show.
type Stats struct {
	Total   int
	Sold    int
	Held    int
	Free    int
	Revenue money.Money
}

// SeatStatus reflects the live availability of a seat.
type SeatStatus string

const (
	SeatFree SeatStatus = "free"
	SeatHeld SeatStatus = "held"
	SeatSold SeatStatus = "sold"
)

// Domain errors the bot expects the Store to return.
var (
	ErrSeatTaken      = errors.New("seat already taken")
	ErrSeatNotFound   = errors.New("seat not found")
	ErrCodeNotFound   = errors.New("reservation code not found")
	ErrAlreadyPaid    = errors.New("reservation already confirmed")
	ErrAlreadyClosed  = errors.New("reservation already closed")
	ErrNotYourBooking = errors.New("reservation belongs to another user")
	ErrShowNotFound   = errors.New("show not found")
)

// Coder mints the short reservation code.
type Coder interface {
	NewCode() (string, error)
}

// Store is the persistence behavior the bot needs.
type Store interface {
	// Shows lists upcoming, non-archived shows — used to render the
	// "афіша" menu when the user runs /start without a deep link.
	Shows(ctx context.Context) ([]Show, error)
	// FindShowBySlug resolves a slug from a "show:<slug>" callback into
	// the full Show record.
	FindShowBySlug(ctx context.Context, slug string) (Show, error)
	// Seats / SeatStatuses / FindFreeSeat / Reserve power the inline-
	// keyboard seat picker — the in-chat alternative to the WebApp Mini
	// App for buyers who prefer to never leave Telegram.
	Seats(ctx context.Context, showID int64) ([]Seat, error)
	SeatStatuses(ctx context.Context, showID int64) (map[int64]SeatStatus, error)
	FindFreeSeat(ctx context.Context, showID int64, row, col int) (Seat, error)
	Reserve(ctx context.Context, seat Seat, tgUserID, tgChatID int64, buyerName, code string, hold time.Duration) (Reservation, error)
	// CreateOrder groups N seats under one payment code. Used by the
	// in-chat multi-seat picker so a single monobank payment covers all
	// the selected seats. Returns ErrSeatTaken / ErrSeatNotSellable on
	// race; the entire order rolls back on failure.
	CreateOrder(ctx context.Context, seats []Seat, tgUserID, tgChatID int64, buyerName, code string, hold time.Duration) (Order, []OrderItem, error)
	CancelReservation(ctx context.Context, code string, tgUserID int64) (Reservation, Seat, error)
	// CancelHeldOrderByUser cascade-cancels a HELD multi-seat order on
	// behalf of its owner. Confirmed orders return ErrAlreadyPaid — bot
	// buyers never get a self-refund path; admin handles those.
	CancelHeldOrderByUser(ctx context.Context, orderCode string, tgUserID int64) ([]Seat, error)
	MyReservations(ctx context.Context, tgUserID int64) ([]MyItem, error)
	Stats(ctx context.Context, showID int64) (Stats, error)
	// LinkOrderToTGChat attaches a Telegram chat to a web-buyer order
	// by its public code so the bot can deliver the PDFs after payment.
	// Returns ErrCodeNotFound for unknown codes, ErrAlreadyClosed for
	// cancelled orders.
	LinkOrderToTGChat(ctx context.Context, code string, tgUserID, tgChatID int64) (Order, []OrderItem, error)
	// LogAudit writes one row to the shared audit_log so journal entries
	// from the bot (order.create from a Telegram buyer, reservation.cancel
	// from the user-cancel button) sit alongside admin / web actions.
	// Empty implementation is OK — caller swallows the error.
	LogAudit(ctx context.Context, action, target, actorLabel, detailsJSON string) error
}

// ReconcileResult is the outcome of a /reconcile sweep.
type ReconcileResult struct {
	Scanned int // transactions examined
	Matched int // tickets newly issued
}

// Reconciler scans recent statement entries and confirms any whose
// reservation code matches an open booking — a rescue net for webhook
// events Mono never delivered.
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

// ShowFn resolves the currently active show. Used by reminders and the
// /stats command for "the most relevant show right now".
type ShowFn func(ctx context.Context) (Show, error)

type Bot struct {
	tb         *tele.Bot
	store      Store
	coder      Coder
	baseURL    string // e.g. https://monokasa.app — used for WebApp deep links
	jarLink    string
	hold       time.Duration
	adminTGID  int64
	reconciler Reconciler   // optional — nil if MONO_TOKEN missing
	jar        JarLookup    // optional — nil if jar link unparseable
	hub        *realtime.Hub // optional — nil-safe Publish, skipped for tests
	showFn     ShowFn
	// onSeatsFreed fires after any user-cancel that releases seats. Main.go
	// wires it to the waitlist notifier (and could add other side effects
	// later). nil-safe: skipped when not configured.
	onSeatsFreed func(ctx context.Context, showID int64, count int)

	// pending tracks chat users mid-pick (accumulating seats in the
	// inline keyboard) or mid-name-input (after tapping "Завершити"). Key
	// = tg user id; value = pendingPick. A periodic sweep drops entries
	// past Until so abandoned picks don't pile up.
	pending sync.Map
	done    chan struct{}
}

// pendingPick is the bot-side basket: the seats this chat user has
// currently ticked in the inline picker, awaiting either more taps, the
// "✅ Завершити" button (→ AwaitingName), or a TTL sweep.
type pendingPick struct {
	ShowID int64
	Slug   string
	// Seats accumulates picks in tap order. Re-tapping a row/col toggles
	// it off. Stored as a slice (not a set) so we render in a stable
	// "first picked first" order in the confirmation message.
	Seats []pickedSeat
	// AwaitingName flips true after "✅ Завершити"; handleText only fires
	// the order creation when this is set. While false, free-text input
	// from the user is ignored.
	AwaitingName bool
	Until        time.Time
}

type pickedSeat struct {
	SeatID int64
	Row    int
	Col    int
	Price  money.Money
}

type Options struct {
	Token      string
	Store      Store
	Coder      Coder
	ShowFn     ShowFn
	BaseURL    string // optional; without it WebApp buttons fall back to plain URL share
	JarLink    string
	Hold       time.Duration // how long a pre-paid hold lives
	AdminTGID  int64
	Reconciler Reconciler    // optional
	Jar        JarLookup     // optional
	Hub        *realtime.Hub // optional; broadcasts seat changes to SSE subscribers
	// OnSeatsFreed fires when a user cancels via /my and seats return to
	// the pool. main.go wires this to the waitlist notifier so the next-
	// in-line buyer gets pinged. nil = no-op.
	OnSeatsFreed func(ctx context.Context, showID int64, count int)
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
		tb: tb, store: opts.Store, coder: opts.Coder, showFn: opts.ShowFn,
		baseURL: strings.TrimRight(opts.BaseURL, "/"),
		jarLink: opts.JarLink, hold: opts.Hold, adminTGID: opts.AdminTGID,
		reconciler: opts.Reconciler, jar: opts.Jar, hub: opts.Hub,
		onSeatsFreed: opts.OnSeatsFreed,
		done:         make(chan struct{}),
	}
	b.routes()
	go b.sweepPending(time.Minute)
	return b, nil
}

// sweepPending drops pending picks whose TTL has lapsed.
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

// pendingTTL bounds how long a user has to type their name after
// picking a seat from the inline keyboard. Same value the old PR #1 bot
// used; long enough to switch apps and come back, short enough to free
// the mental hold.
const pendingTTL = 10 * time.Minute

func (b *Bot) Start() { b.tb.Start() }

func (b *Bot) Stop() {
	select {
	case <-b.done:
	default:
		close(b.done)
	}
	b.tb.Stop()
}

// SetReconciler wires the rescue-net scanner. Pass nil to disable.
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

// SendCancellation pings a buyer that the admin force-cancelled their
// reservation. Best-effort: any send error gets logged by the caller —
// the DB row is already cancelled, the message is courtesy.
func (b *Bot) SendCancellation(chatID int64, seat Seat) error {
	_, err := b.tb.Send(tele.ChatID(chatID), fmt.Sprintf(
		"❌ Твою бронь скасовано адміністратором.\nРяд %d, місце %d.\n"+
			"Якщо оплата вже пройшла — гроші повернуть руками через моно. "+
			"Питання — напиши організатору.",
		seat.Row, seat.Col))
	return err
}

// NotifyShowSoon pings a buyer about the upcoming show. Title is fetched
// fresh so an admin rename right before show-time lands in the reminder.
func (b *Bot) NotifyShowSoon(chatID int64, seat Seat, when time.Time) error {
	title := "захід"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if sh, err := b.showFn(ctx); err == nil {
		title = sh.Title
	}
	_, err := b.tb.Send(tele.ChatID(chatID), fmt.Sprintf(
		"Привіт! Нагадую: %s — вже сьогодні о %s.\nТвоє місце: ряд %d · %d.\nЧекаємо!",
		title, formatClock(when), seat.Row, seat.Col))
	return err
}

func (b *Bot) routes() {
	b.tb.Handle("/start", b.handleStart)
	b.tb.Handle("/help", b.handleStart)
	b.tb.Handle("/events", b.handleEvents)
	b.tb.Handle("/seats", b.handleEvents) // legacy alias
	b.tb.Handle("/my", b.handleMy)
	b.tb.Handle("/stats", b.handleStats)
	b.tb.Handle("/reconcile", b.handleReconcile)
	b.tb.Handle("/jar", b.handleJar)
	b.tb.Handle(tele.OnText, b.handleText)
	b.tb.Handle(tele.OnCallback, b.handleCallback)
}

// --- /start ---

func (b *Bot) handleStart(c tele.Context) error {
	// Deep-link from public web flow: /start res_<code> attaches this
	// chat to that reservation so the bot delivers the PDF on payment.
	payload := strings.TrimSpace(c.Message().Payload)
	if rest, ok := strings.CutPrefix(payload, "res_"); ok && rest != "" {
		return b.linkReservation(c, rest)
	}
	return b.sendEventList(c, "Привіт! Це бот продажу квитків. Обери подію:")
}

func (b *Bot) handleEvents(c tele.Context) error {
	return b.sendEventList(c, "📅 Афіша:")
}

// sendEventList renders the upcoming-shows menu as an inline keyboard.
// Each button carries "show:<slug>" so the callback handler can fetch
// fresh data on tap (titles can change in the admin web).
func (b *Bot) sendEventList(c tele.Context, header string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	shows, err := b.store.Shows(ctx)
	if err != nil {
		slog.Error("list shows", "err", err)
		return c.Send("Внутрішня помилка, спробуй пізніше.")
	}
	if len(shows) == 0 {
		return c.Send("Зараз подій немає. Загляни пізніше 🎭")
	}

	markup := &tele.ReplyMarkup{}
	rows := make([][]tele.InlineButton, 0, len(shows))
	for _, sh := range shows {
		rows = append(rows, []tele.InlineButton{{
			Unique: "show",
			Text:   fmt.Sprintf("%s · %s", sh.Title, formatDateShort(sh.StartsAt)),
			Data:   sh.Slug,
		}})
	}
	markup.InlineKeyboard = rows
	return c.Send(header, markup)
}

// --- callbacks ---

func (b *Bot) handleCallback(c tele.Context) error {
	cb := c.Callback()
	if cb == nil {
		return nil
	}
	switch {
	case strings.HasPrefix(cb.Data, "\fshow|"):
		return b.callbackShow(c, cb)
	case strings.HasPrefix(cb.Data, "\fpick|"):
		return b.callbackPick(c, cb)
	case strings.HasPrefix(cb.Data, "\fseat|"):
		return b.callbackSeat(c, cb)
	case strings.HasPrefix(cb.Data, "\fdone|"):
		return b.callbackDone(c, cb)
	case strings.HasPrefix(cb.Data, "\fclear|"):
		return b.callbackClear(c, cb)
	case strings.HasPrefix(cb.Data, "\fcancel|"):
		return b.callbackCancel(c, cb)
	case strings.HasPrefix(cb.Data, "\fcanord|"):
		return b.callbackCancelOrder(c, cb)
	case strings.HasPrefix(cb.Data, "\fevents|"):
		_ = c.Respond(&tele.CallbackResponse{})
		return b.sendEventList(c, "📅 Афіша:")
	}
	return c.Respond(&tele.CallbackResponse{Text: "unknown action"})
}

// callbackShow renders the per-event menu after the user tapped a show
// from the афіша list. The "Обрати місце" button is a Telegram WebApp
// button → opens /event/<slug> as a Mini App. Falls back to plain link
// when BASE_URL isn't configured.
func (b *Bot) callbackShow(c tele.Context, cb *tele.Callback) error {
	slug := strings.TrimPrefix(cb.Data, "\fshow|")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sh, err := b.store.FindShowBySlug(ctx, slug)
	if errors.Is(err, ErrShowNotFound) {
		return c.Respond(&tele.CallbackResponse{Text: "Подію не знайдено"})
	}
	if err != nil {
		slog.Error("find show", "slug", slug, "err", err)
		return c.Respond(&tele.CallbackResponse{Text: "Помилка"})
	}
	_ = c.Respond(&tele.CallbackResponse{})

	// Two ways to pick: inline keyboard (default, never leaves chat)
	// and — if BASE_URL is set — WebApp button that opens the SVG map
	// as a Mini App for users who prefer the visual experience.
	markup := &tele.ReplyMarkup{}
	rows := [][]tele.InlineButton{
		{{Unique: "pick", Text: "📋 Обрати місце", Data: sh.Slug}},
	}
	// Telegram WebApp URLs MUST be https://. http://localhost is fine
	// for dev with the SPA in a browser, but TG would reject the
	// inline-keyboard button with a 400. Skip the button rather than
	// crashing the whole message; the in-chat "📋 Обрати місце" path
	// still works.
	if strings.HasPrefix(b.baseURL, "https://") {
		rows = append(rows, []tele.InlineButton{{
			Text:   "🗺 Відкрити мапу залу",
			WebApp: &tele.WebApp{URL: fmt.Sprintf("%s/event/%s", b.baseURL, sh.Slug)},
		}})
	}
	rows = append(rows, []tele.InlineButton{
		{Unique: "events", Text: "↩ До списку", Data: "back"},
	})
	markup.InlineKeyboard = rows

	text := fmt.Sprintf("🎭 *%s*\n📅 %s\n",
		escapeMarkdown(sh.Title), formatDateTime(sh.StartsAt))
	if sh.Venue != "" {
		text += "📍 " + escapeMarkdown(sh.Venue) + "\n"
	}
	if sh.Description != "" {
		text += "\n" + escapeMarkdown(sh.Description) + "\n"
	}
	return c.Send(text, tele.ModeMarkdown, markup)
}

// callbackPick handles the "📋 Обрати місце" tap from the show menu —
// renders the seat grid as an inline keyboard. The grid uses the same
// row/col layout the admin set up; free seats show their col number,
// held are "…", sold are "✖". Re-entering the picker wipes any prior
// in-progress selection for this user so the basket always starts
// empty here.
func (b *Bot) callbackPick(c tele.Context, cb *tele.Callback) error {
	slug := strings.TrimPrefix(cb.Data, "\fpick|")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sh, err := b.store.FindShowBySlug(ctx, slug)
	if errors.Is(err, ErrShowNotFound) {
		return c.Respond(&tele.CallbackResponse{Text: "Подію не знайдено"})
	}
	if err != nil {
		slog.Error("pick: find show", "slug", slug, "err", err)
		return c.Respond(&tele.CallbackResponse{Text: "Помилка"})
	}
	seats, err := b.store.Seats(ctx, sh.ID)
	if err != nil {
		slog.Error("pick: list seats", "showId", sh.ID, "err", err)
		return c.Respond(&tele.CallbackResponse{Text: "Помилка"})
	}
	status, err := b.store.SeatStatuses(ctx, sh.ID)
	if err != nil {
		slog.Error("pick: seat statuses", "showId", sh.ID, "err", err)
		return c.Respond(&tele.CallbackResponse{Text: "Помилка"})
	}
	_ = c.Respond(&tele.CallbackResponse{})

	if !anySellable(seats) {
		return c.Send("Зала ще не налаштована. Спробуй пізніше.")
	}

	// Re-enter the picker → wipe any half-done basket from earlier.
	b.pending.Store(cb.Sender.ID, pendingPick{
		ShowID: sh.ID,
		Slug:   sh.Slug,
		Until:  time.Now().Add(pendingTTL),
	})

	text, markup := renderPickBoard(sh.Slug, seats, status, nil)
	return c.Send(text, markup)
}

// maxPickSeats caps how many seats one chat user can stack in the
// in-chat picker. Mirrors the soft cap on POST /api/public/orders so the
// bot and web paths share the same upper bound.
const maxPickSeats = 20

// renderPickBoard builds the inline-keyboard seat grid plus the footer
// controls ("✅ Завершити", "🧹 Очистити", "↩ Назад"). Seats in `picked`
// render with a "✓" instead of the column number so the user can spot
// what's in their basket without leaving the message.
func renderPickBoard(slug string, seats []Seat, status map[int64]SeatStatus, picked []pickedSeat) (string, *tele.ReplyMarkup) {
	pickedSet := make(map[int64]struct{}, len(picked))
	for _, p := range picked {
		pickedSet[p.SeatID] = struct{}{}
	}

	rowMap := make(map[int][]Seat)
	maxRow := 0
	for _, s := range seats {
		if !s.Sellable {
			continue
		}
		rowMap[s.Row] = append(rowMap[s.Row], s)
		if s.Row > maxRow {
			maxRow = s.Row
		}
	}

	kb := make([][]tele.InlineButton, 0, maxRow+3)
	for r := 1; r <= maxRow; r++ {
		rowSeats := rowMap[r]
		btns := make([]tele.InlineButton, 0, len(rowSeats))
		for _, s := range rowSeats {
			label := strconv.Itoa(s.Col)
			switch status[s.ID] {
			case SeatSold:
				label = "✖"
			case SeatHeld:
				if _, ok := pickedSet[s.ID]; !ok {
					label = "…"
				}
			}
			if _, ok := pickedSet[s.ID]; ok {
				// Picked overrides everything else (the seat is in the
				// caller's own basket — still free from the DB's POV).
				label = "✓"
			}
			btns = append(btns, tele.InlineButton{
				Unique: "seat",
				Text:   label,
				Data:   fmt.Sprintf("%s:%d:%d", slug, s.Row, s.Col),
			})
		}
		kb = append(kb, btns)
	}

	// Footer: dynamic "Завершити вибір (N)", optional Clear, then Back.
	doneLabel := "✅ Завершити вибір"
	if n := len(picked); n > 0 {
		doneLabel = fmt.Sprintf("✅ Завершити (%d · %s)", n, sumPrice(picked).String())
	}
	footer := []tele.InlineButton{
		{Unique: "done", Text: doneLabel, Data: slug},
	}
	if len(picked) > 0 {
		footer = append(footer, tele.InlineButton{
			Unique: "clear", Text: "🧹 Очистити", Data: slug,
		})
	}
	kb = append(kb, footer)
	kb = append(kb, []tele.InlineButton{
		{Unique: "show", Text: "↩ Назад", Data: slug},
	})

	header := "🎭 ━━━━━ СЦЕНА ━━━━━ 🎭\n               ▲ ближче до сцени\n\n"
	body := "Ряд 1 — найближче до сцени.\nНатисни вільні місця (можна декілька), потім ✅ Завершити."
	if n := len(picked); n > 0 {
		body += fmt.Sprintf("\n\nВ кошику: %d · %s", n, sumPrice(picked).String())
	}
	return header + body, &tele.ReplyMarkup{InlineKeyboard: kb}
}

func anySellable(seats []Seat) bool {
	for _, s := range seats {
		if s.Sellable {
			return true
		}
	}
	return false
}

func sumPrice(picks []pickedSeat) money.Money {
	if len(picks) == 0 {
		return money.Money{}
	}
	out := money.Money{Code: picks[0].Price.Code}
	for _, p := range picks {
		out.Minor += p.Price.Minor
	}
	return out
}

// callbackSeat handles a tap on one of the seat buttons. Toggles the
// seat in the user's pending basket and re-renders the keyboard
// in-place. Reaching the maxPickSeats cap shows a transient toast and
// leaves the basket unchanged.
func (b *Bot) callbackSeat(c tele.Context, cb *tele.Callback) error {
	data := strings.TrimPrefix(cb.Data, "\fseat|")
	parts := strings.SplitN(data, ":", 3)
	if len(parts) != 3 {
		return c.Respond(&tele.CallbackResponse{Text: "bad seat"})
	}
	slug := parts[0]
	row, err1 := strconv.Atoi(parts[1])
	col, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil {
		return c.Respond(&tele.CallbackResponse{Text: "bad seat"})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sh, err := b.store.FindShowBySlug(ctx, slug)
	if err != nil {
		return c.Respond(&tele.CallbackResponse{Text: "Подію не знайдено"})
	}

	pick, ok := b.loadPending(cb.Sender.ID, sh)
	if !ok {
		// Stale message: their basket either expired or never existed
		// for this show. Bounce them back to the picker so it re-seeds.
		return c.Respond(&tele.CallbackResponse{Text: "Натисни 📋 Обрати місце ще раз"})
	}

	// If the seat is already in the basket, toggle off without hitting
	// the DB. Otherwise we need the live seat state to decide whether
	// the tap should add or refuse.
	if idx := indexOfPick(pick.Seats, row, col); idx >= 0 {
		pick.Seats = append(pick.Seats[:idx], pick.Seats[idx+1:]...)
		pick.Until = time.Now().Add(pendingTTL)
		b.pending.Store(cb.Sender.ID, pick)
		_ = c.Respond(&tele.CallbackResponse{})
		return b.editPickBoard(c, sh, pick.Seats)
	}

	if len(pick.Seats) >= maxPickSeats {
		return c.Respond(&tele.CallbackResponse{
			Text: fmt.Sprintf("Максимум %d місць за одну покупку", maxPickSeats),
		})
	}

	seat, err := b.store.FindFreeSeat(ctx, sh.ID, row, col)
	if err != nil {
		return c.Respond(&tele.CallbackResponse{Text: friendly(err)})
	}
	pick.Seats = append(pick.Seats, pickedSeat{
		SeatID: seat.ID, Row: seat.Row, Col: seat.Col, Price: seat.Price,
	})
	pick.Until = time.Now().Add(pendingTTL)
	b.pending.Store(cb.Sender.ID, pick)
	_ = c.Respond(&tele.CallbackResponse{})
	return b.editPickBoard(c, sh, pick.Seats)
}

// callbackClear empties the current basket but keeps the user on the
// picker — re-renders the board without any "✓" markers.
func (b *Bot) callbackClear(c tele.Context, cb *tele.Callback) error {
	slug := strings.TrimPrefix(cb.Data, "\fclear|")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sh, err := b.store.FindShowBySlug(ctx, slug)
	if err != nil {
		return c.Respond(&tele.CallbackResponse{Text: "Подію не знайдено"})
	}
	pick, ok := b.loadPending(cb.Sender.ID, sh)
	if !ok {
		return c.Respond(&tele.CallbackResponse{Text: "Натисни 📋 Обрати місце ще раз"})
	}
	pick.Seats = nil
	pick.AwaitingName = false
	pick.Until = time.Now().Add(pendingTTL)
	b.pending.Store(cb.Sender.ID, pick)
	_ = c.Respond(&tele.CallbackResponse{Text: "Очищено"})
	return b.editPickBoard(c, sh, nil)
}

// callbackDone closes the picking phase: validates ≥1 seat is in the
// basket, flips AwaitingName, and asks for the buyer's name via
// ForceReply. handleText finishes the flow when the user replies.
func (b *Bot) callbackDone(c tele.Context, cb *tele.Callback) error {
	slug := strings.TrimPrefix(cb.Data, "\fdone|")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sh, err := b.store.FindShowBySlug(ctx, slug)
	if err != nil {
		return c.Respond(&tele.CallbackResponse{Text: "Подію не знайдено"})
	}
	pick, ok := b.loadPending(cb.Sender.ID, sh)
	if !ok {
		return c.Respond(&tele.CallbackResponse{Text: "Натисни 📋 Обрати місце ще раз"})
	}
	if len(pick.Seats) == 0 {
		return c.Respond(&tele.CallbackResponse{Text: "Спочатку обери хоч одне місце"})
	}
	pick.AwaitingName = true
	pick.Until = time.Now().Add(pendingTTL)
	b.pending.Store(cb.Sender.ID, pick)
	_ = c.Respond(&tele.CallbackResponse{})

	var summary strings.Builder
	for _, p := range pick.Seats {
		fmt.Fprintf(&summary, "· ряд %d місце %d — %s\n", p.Row, p.Col, p.Price)
	}
	prompt := fmt.Sprintf(
		"Обрано %d %s:\n%sСума: %s.\n\nВведи ім'я та прізвище — будуть на квитках:",
		len(pick.Seats), seatWord(len(pick.Seats)), summary.String(),
		sumPrice(pick.Seats))
	_, err = b.tb.Send(cb.Sender, prompt,
		&tele.ReplyMarkup{ForceReply: true, Selective: true})
	return err
}

func (b *Bot) handleText(c tele.Context) error {
	sender := c.Sender()
	if sender == nil {
		return nil
	}
	raw, ok := b.pending.Load(sender.ID)
	if !ok {
		return nil // user typed text but isn't in any pick flow
	}
	pick, ok := raw.(pendingPick)
	if !ok {
		b.pending.Delete(sender.ID)
		return nil
	}
	if !pick.AwaitingName {
		return nil // still picking — free-text shouldn't derail the picker
	}
	if time.Now().After(pick.Until) {
		b.pending.Delete(sender.ID)
		return c.Send("Час на введення імені вийшов. Тицьни /events і вибери місця ще раз.")
	}

	name, err := normalizeName(c.Text())
	if err != nil {
		return c.Send(err.Error() + "\nСпробуй ще раз:")
	}
	if len(pick.Seats) == 0 {
		b.pending.Delete(sender.ID)
		return c.Send("Кошик порожній. Тицьни /events і вибери місця.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Re-resolve each pick to a live Seat — gives a friendly error if
	// someone else grabbed it between picking and confirming. CreateOrder
	// re-checks atomically inside the tx, so this is just for UX.
	seats := make([]Seat, 0, len(pick.Seats))
	for _, p := range pick.Seats {
		s, err := b.store.FindFreeSeat(ctx, pick.ShowID, p.Row, p.Col)
		if err != nil {
			b.pending.Delete(sender.ID)
			return c.Send(fmt.Sprintf("Місце ряд %d · %d: %s",
				p.Row, p.Col, friendly(err)))
		}
		seats = append(seats, s)
	}

	code, err := b.coder.NewCode()
	if err != nil {
		return c.Send("Внутрішня помилка, спробуй пізніше")
	}
	order, items, err := b.store.CreateOrder(ctx, seats, sender.ID, c.Chat().ID, name, code, b.hold)
	if err != nil {
		b.pending.Delete(sender.ID)
		return c.Send(friendly(err))
	}
	b.pending.Delete(sender.ID)

	// Broadcast each newly-held seat to live SSE subscribers so any
	// open web map flips this seat to "taken" without a refresh.
	for _, it := range items {
		b.hub.Publish(it.Seat.ShowID, realtime.Event{
			Type: "seat_status", SeatID: it.Seat.ID, Status: realtime.SeatHeld,
		})
	}
	// Audit: one row per bot-side order. Buyer label is the Telegram
	// username when present, else falls back to the buyer name typed
	// at the ForceReply prompt.
	actor := name
	if sender.Username != "" {
		actor = "@" + sender.Username
	}
	auditDetails, _ := json.Marshal(map[string]any{
		"code": order.Code, "seats": len(items),
		"buyer_name": name, "tg_user_id": sender.ID,
		"source": "bot",
	})
	if err := b.store.LogAudit(ctx, "order.create",
		fmt.Sprintf("order:%d", order.ID), actor, string(auditDetails)); err != nil {
		slog.Error("bot audit write failed", "code", order.Code, "err", err)
	}

	total := sumOrderPrice(items)
	payURL := jarPrefillURL(b.jarLink, total, order.Code)

	var seatsList strings.Builder
	for _, it := range items {
		fmt.Fprintf(&seatsList, "· ряд %d місце %d — %s\n", it.Seat.Row, it.Seat.Col, it.Seat.Price)
	}

	// Cancel button only on single-seat (legacy parity). Multi-seat
	// cancel goes through /my so each row has its own ✖ — clearer than
	// guessing which seat a single button represents.
	rows := [][]tele.InlineButton{
		{{Text: "💳 Оплатити", URL: payURL}},
	}
	if len(items) == 1 {
		rows = append(rows, []tele.InlineButton{
			{Unique: "cancel", Text: "✖ Скасувати бронь", Data: items[0].Reservation.Code},
		})
	}
	markup := &tele.ReplyMarkup{InlineKeyboard: rows}

	// All child reservations share the order's expiry (set atomically in
	// CreateOrder); pull from the first row.
	expires := items[0].Reservation.ExpiresAt

	var msg string
	if len(items) == 1 {
		s := items[0].Seat
		msg = fmt.Sprintf(
			"Місце забронювано: ряд %d, місце %d.\n"+
				"На квитку буде: *%s*\n\n"+
				"💳 Натисни *Оплатити* — сума й коментар вже вписані.\n"+
				"Код у коментарі — `%s`.\n"+
				"Бронювання дійсне до %s. Після оплати бот сам пришле PDF.",
			s.Row, s.Col, name, order.Code, formatClock(expires))
	} else {
		msg = fmt.Sprintf(
			"Забронював %d %s:\n%s"+
				"На квитках буде: *%s*\n\n"+
				"💳 Натисни *Оплатити* — сума %s і коментар вже вписані.\n"+
				"Код у коментарі — `%s`.\n"+
				"Бронювання дійсне до %s. Після оплати бот пришле %d PDF — по одному на місце.",
			len(items), seatWord(len(items)), seatsList.String(),
			name, total, order.Code, formatClock(expires), len(items))
	}

	return c.Send(msg, tele.ModeMarkdown,
		&tele.SendOptions{DisableWebPagePreview: true}, markup)
}

// loadPending reads a basket out of the sync.Map, verifying it still
// belongs to the right show and hasn't expired. Stale or mismatched
// entries are deleted and reported as not-found.
func (b *Bot) loadPending(userID int64, sh Show) (pendingPick, bool) {
	raw, ok := b.pending.Load(userID)
	if !ok {
		return pendingPick{}, false
	}
	pick, ok := raw.(pendingPick)
	if !ok {
		b.pending.Delete(userID)
		return pendingPick{}, false
	}
	if time.Now().After(pick.Until) {
		b.pending.Delete(userID)
		return pendingPick{}, false
	}
	if pick.Slug != sh.Slug {
		// Different show — silently let the caller decide. We don't
		// delete: the basket for the other show stays valid.
		return pendingPick{}, false
	}
	return pick, true
}

// editPickBoard re-renders the picker keyboard in-place for the message
// the callback fired from. Falls back to sending a fresh message if the
// edit fails (e.g. message too old to edit per Telegram API limits).
func (b *Bot) editPickBoard(c tele.Context, sh Show, picked []pickedSeat) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	seats, err := b.store.Seats(ctx, sh.ID)
	if err != nil {
		slog.Error("editPickBoard: list seats", "showId", sh.ID, "err", err)
		return c.Send("Внутрішня помилка, спробуй ще раз пізніше.")
	}
	status, err := b.store.SeatStatuses(ctx, sh.ID)
	if err != nil {
		slog.Error("editPickBoard: seat statuses", "showId", sh.ID, "err", err)
		return c.Send("Внутрішня помилка, спробуй ще раз пізніше.")
	}
	text, markup := renderPickBoard(sh.Slug, seats, status, picked)
	if editErr := c.Edit(text, markup); editErr != nil {
		// Message too old / unchanged content — send a new one as
		// fallback so the user always sees their updated basket.
		return c.Send(text, markup)
	}
	return nil
}

func indexOfPick(picks []pickedSeat, row, col int) int {
	for i, p := range picks {
		if p.Row == row && p.Col == col {
			return i
		}
	}
	return -1
}

func sumOrderPrice(items []OrderItem) money.Money {
	if len(items) == 0 {
		return money.Money{}
	}
	out := money.Money{Code: items[0].Seat.Price.Code}
	for _, it := range items {
		out.Minor += it.Seat.Price.Minor
	}
	return out
}

func seatWord(n int) string {
	switch {
	case n == 1:
		return "місце"
	case n >= 2 && n <= 4:
		return "місця"
	default:
		return "місць"
	}
}

// friendly turns store-side errors into user-facing Ukrainian.
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
	// Single-seat path — cancel button only renders on N=1 orders.
	// Broadcast the freed seat to live SSE subscribers.
	b.hub.Publish(seat.ShowID, realtime.Event{
		Type: "seat_status", SeatID: seat.ID, Status: realtime.SeatFree,
	})
	if b.onSeatsFreed != nil {
		b.onSeatsFreed(ctx, seat.ShowID, 1)
	}
	_, err = b.tb.Send(cb.Sender,
		fmt.Sprintf("Бронь ряд %d місце %d скасовано. Місце знов вільне.", seat.Row, seat.Col))
	return err
}

// --- /start res_<code> deep-link from web-buyer ---

func (b *Bot) linkReservation(c tele.Context, code string) error {
	sender := c.Sender()
	if sender == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	order, items, err := b.store.LinkOrderToTGChat(ctx, code, sender.ID, c.Chat().ID)
	switch {
	case errors.Is(err, ErrCodeNotFound):
		return c.Send("Цю бронь не знайдено. Можливо, посилання застаріле.")
	case errors.Is(err, ErrAlreadyClosed):
		return c.Send("Цю бронь вже скасовано.")
	case errors.Is(err, ErrNotYourBooking):
		// Another TG user already linked this order — refuse takeover
		// so a leaked code can't redirect ticket delivery.
		slog.Warn("link order: refused — already linked to another user",
			"code", code, "sender", sender.ID)
		return c.Send("Цю бронь вже прив'язано до іншого користувача. " +
			"Якщо це твоя бронь і ти втратив доступ — напиши організатору.")
	case err != nil:
		slog.Error("link order", "code", code, "err", err)
		return c.Send("Внутрішня помилка, спробуй пізніше.")
	}

	var seatList strings.Builder
	for i, it := range items {
		if i > 0 {
			seatList.WriteString(", ")
		}
		fmt.Fprintf(&seatList, "ряд %d місце %d", it.Seat.Row, it.Seat.Col)
	}
	if order.ConfirmedAt != nil {
		return c.Send(fmt.Sprintf(
			"Бронь вже оплачена 🎉\nМісця: %s.\nКвитки прийшли на email — перевір пошту.",
			seatList.String()))
	}
	return c.Send(fmt.Sprintf(
		"Підключив цю бронь до Telegram ✅\nМісця: %s.\nПісля оплати квитки прийдуть сюди (і на email також).",
		seatList.String()))
}

// --- /my ---

// orderCodeOf strips the ".N" multi-seat suffix from a reservation code,
// returning the parent order code. Single-seat codes round-trip unchanged.
func orderCodeOf(reservationCode string) string {
	if i := strings.IndexByte(reservationCode, '.'); i > 0 {
		return reservationCode[:i]
	}
	return reservationCode
}

// myGroup is a ready-to-render bucket of one order's reservations.
type myGroup struct {
	OrderCode string
	Items     []MyItem // sorted by row/col for stable rendering
	Confirmed bool     // any reservation confirmed → whole order is paid
	Expired   bool     // none confirmed AND all expired
	ExpiresAt time.Time
}

func (b *Bot) handleMy(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	items, err := b.store.MyReservations(ctx, c.Sender().ID)
	if err != nil {
		slog.Error("/my: list reservations", "tgUserId", c.Sender().ID, "err", err)
		return c.Send("Внутрішня помилка, спробуй ще раз пізніше.")
	}
	if len(items) == 0 {
		return c.Send("У тебе ще немає бронювань. /events — афіша.")
	}

	// Group reservations by order code so multi-seat orders render as a
	// single block with one cancel button rather than N rows.
	byOrder := make(map[string]*myGroup)
	for _, it := range items {
		code := orderCodeOf(it.Reservation.Code)
		g, ok := byOrder[code]
		if !ok {
			g = &myGroup{OrderCode: code, ExpiresAt: it.Reservation.ExpiresAt}
			byOrder[code] = g
		}
		g.Items = append(g.Items, it)
	}
	now := time.Now()
	for _, g := range byOrder {
		anyConfirmed := false
		anyLive := false
		for _, it := range g.Items {
			if it.Reservation.ConfirmedAt != nil {
				anyConfirmed = true
			}
			if it.Reservation.ConfirmedAt == nil && it.Reservation.ExpiresAt.After(now) {
				anyLive = true
			}
		}
		g.Confirmed = anyConfirmed
		g.Expired = !anyConfirmed && !anyLive
	}
	// Stable order: live (held) first, then confirmed, then expired.
	// Within each bucket: by earliest expiry.
	codes := make([]string, 0, len(byOrder))
	for k := range byOrder {
		codes = append(codes, k)
	}
	sort.SliceStable(codes, func(i, j int) bool {
		a, b := byOrder[codes[i]], byOrder[codes[j]]
		rank := func(g *myGroup) int {
			switch {
			case !g.Confirmed && !g.Expired:
				return 0
			case g.Confirmed:
				return 1
			default:
				return 2
			}
		}
		ra, rb := rank(a), rank(b)
		if ra != rb {
			return ra < rb
		}
		return a.ExpiresAt.Before(b.ExpiresAt)
	})

	// One message per order keeps the cancel button anchored to the right
	// rows — Telegram inline keyboards live on a single message at a time.
	intro := "Твої бронювання:"
	if err := c.Send(intro); err != nil {
		return err
	}
	for _, code := range codes {
		g := byOrder[code]
		var sb strings.Builder
		for _, it := range g.Items {
			fmt.Fprintf(&sb, "· ряд %d місце %d\n", it.Seat.Row, it.Seat.Col)
		}
		seatsBlock := sb.String()

		switch {
		case g.Confirmed:
			msg := fmt.Sprintf("✅ Оплачено · код `%s`\n%s",
				escapeMarkdown(code), seatsBlock)
			if err := c.Send(msg, tele.ModeMarkdown); err != nil {
				return err
			}
		case g.Expired:
			msg := fmt.Sprintf("✖ Протермінувалось · код `%s`\n%s",
				escapeMarkdown(code), seatsBlock)
			if err := c.Send(msg, tele.ModeMarkdown); err != nil {
				return err
			}
		default:
			// Held. Single ✖ Скасувати button regardless of seat count —
			// for multi-seat it cascades; for single-seat it's the same
			// reservation code so callbackCancelOrder hits one row anyway.
			msg := fmt.Sprintf("⏳ Чекає оплати до %s · код `%s`\n%s",
				formatClock(g.ExpiresAt), escapeMarkdown(code), seatsBlock)
			cancelLabel := "✖ Скасувати бронь"
			if len(g.Items) > 1 {
				cancelLabel = fmt.Sprintf("✖ Скасувати замовлення (%d місця)", len(g.Items))
			}
			markup := &tele.ReplyMarkup{
				InlineKeyboard: [][]tele.InlineButton{
					{{Unique: "canord", Text: cancelLabel, Data: code}},
				},
			}
			if err := c.Send(msg, tele.ModeMarkdown, markup); err != nil {
				return err
			}
		}
	}
	return nil
}

// callbackCancelOrder handles "✖ Скасувати …" from /my for held orders.
// Cascades the whole order — multi-seat partial-cancel is forbidden mid-
// payment because the buyer already locked the total in monobank.
func (b *Bot) callbackCancelOrder(c tele.Context, cb *tele.Callback) error {
	code := strings.TrimPrefix(cb.Data, "\fcanord|")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	freed, err := b.store.CancelHeldOrderByUser(ctx, code, cb.Sender.ID)
	if err != nil {
		switch {
		case errors.Is(err, ErrCodeNotFound), errors.Is(err, ErrAlreadyClosed):
			return c.Respond(&tele.CallbackResponse{Text: "Цю бронь вже закрито"})
		case errors.Is(err, ErrAlreadyPaid):
			return c.Respond(&tele.CallbackResponse{Text: "Вже оплачено — повернення тільки руками"})
		case errors.Is(err, ErrNotYourBooking):
			return c.Respond(&tele.CallbackResponse{Text: "Це не твоя бронь"})
		default:
			slog.Error("cancel order", "code", code, "err", err)
			return c.Respond(&tele.CallbackResponse{Text: "Помилка"})
		}
	}
	_ = c.Respond(&tele.CallbackResponse{Text: "Скасовано"})
	// Broadcast every freed seat to live SSE subscribers so anyone
	// staring at the seat map sees them turn green immediately, and
	// count per-show to fire one waitlist notify per affected event.
	perShow := make(map[int64]int, 1)
	for _, s := range freed {
		b.hub.Publish(s.ShowID, realtime.Event{
			Type: "seat_status", SeatID: s.ID, Status: realtime.SeatFree,
		})
		perShow[s.ShowID]++
	}
	if b.onSeatsFreed != nil {
		for showID, count := range perShow {
			b.onSeatsFreed(ctx, showID, count)
		}
	}
	// Edit the original message: drop the keyboard, add a strikethrough-
	// style confirmation. tele.EditReplyMarkup is the lightweight path.
	_, _ = b.tb.Edit(cb.Message,
		fmt.Sprintf("✖ Скасовано · код `%s`\n(%d %s повернулись у пул)",
			escapeMarkdown(code), len(freed), seatWord(len(freed))),
		tele.ModeMarkdown,
		&tele.ReplyMarkup{InlineKeyboard: nil})
	return nil
}

// --- /stats (admin) ---

func (b *Bot) handleStats(c tele.Context) error {
	if b.adminTGID == 0 || c.Sender().ID != b.adminTGID {
		return c.Send("⛔️ команда тільки для адміна")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	show, friendlyErr := b.activeShow(ctx)
	if friendlyErr != "" {
		return c.Send(friendlyErr)
	}
	st, err := b.store.Stats(ctx, show.ID)
	if err != nil {
		slog.Error("/stats: load", "showId", show.ID, "err", err)
		return c.Send("Внутрішня помилка, спробуй ще раз пізніше.")
	}
	return c.Send(fmt.Sprintf(
		"📊 *%s*\n"+
			"%s · %s\n\n"+
			"всього місць: *%d*\n"+
			"продано: *%d*\n"+
			"в очікуванні оплати: *%d*\n"+
			"вільно: *%d*\n\n"+
			"виторг: *%s*",
		escapeMarkdown(show.Title), escapeMarkdown(show.Venue), formatDateTime(show.StartsAt),
		st.Total, st.Sold, st.Held, st.Free, st.Revenue), tele.ModeMarkdown)
}

// activeShow resolves the show with a short context so a hung DB doesn't
// freeze the bot handler. Returns friendly user-facing text on error.
func (b *Bot) activeShow(ctx context.Context) (Show, string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	sh, err := b.showFn(ctx)
	if err != nil {
		return Show{}, "Зараз немає активної події."
	}
	return sh, ""
}

// --- /reconcile (admin) ---

func (b *Bot) handleReconcile(c tele.Context) error {
	if b.adminTGID == 0 || c.Sender().ID != b.adminTGID {
		return c.Send("⛔️ команда тільки для адміна")
	}
	if b.reconciler == nil {
		return c.Send("Реконсайл недоступний: MONO_TOKEN не налаштований.")
	}
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
		slog.Warn("reconcile ack", "err", err)
	}
	progress := func(s string) {
		if progressMsg == nil {
			return
		}
		if _, err := b.tb.Edit(progressMsg, "⏳ "+s); err != nil {
			slog.Warn("reconcile progress edit", "err", err)
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

// --- /jar (admin) ---

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
	out := fmt.Sprintf("🏦 *%s*\n", escapeMarkdown(info.Title))
	if info.Owner != "" {
		out += "власник: " + escapeMarkdown(info.Owner) + "\n"
	}
	out += "зібрано: *" + info.Balance.String() + "*"
	if !info.Goal.IsZero() {
		out += "\nціль: " + info.Goal.String()
	}
	return c.Send(out, tele.ModeMarkdown)
}

// --- name normalisation (still used by validation of buyer name fields
// passed in elsewhere; keep for backward compat with /reserve flow if it
// ever returns to the bot). ---

const nameMaxRunes = 60

// normalizeName trims, collapses spaces and enforces 2..nameMaxRunes rune
// length. Letters, digits, spaces, apostrophes, hyphens and dots are
// accepted — enough for Ukrainian double-barrelled names.
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

// jarPrefillURL appends ?a=<amount>&t=<comment> to the jar share URL so
// monobank opens with sum and comment pre-filled. Currency-aware: UAH
// renders as "250" / "250.99"; JPY as a bare integer (no decimals).
func jarPrefillURL(base string, price money.Money, comment string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	mpm := price.Code.MinorPerMajor()
	dec := price.Code.Decimals()
	var amount string
	switch {
	case mpm <= 1:
		amount = strconv.FormatInt(price.Minor, 10)
	case price.Minor%mpm == 0:
		amount = strconv.FormatInt(price.Minor/mpm, 10)
	default:
		amount = fmt.Sprintf("%d.%0*d", price.Minor/mpm, dec, price.Minor%mpm)
	}
	q := u.Query()
	q.Set("a", amount)
	q.Set("t", comment)
	u.RawQuery = q.Encode()
	return u.String()
}

// --- formatters ---

var ukMonthsGenitive = [...]string{
	"січня", "лютого", "березня", "квітня", "травня", "червня",
	"липня", "серпня", "вересня", "жовтня", "листопада", "грудня",
}

func formatDateTime(t time.Time) string {
	return fmt.Sprintf("%d %s %d · %s",
		t.Day(), ukMonthsGenitive[t.Month()-1], t.Year(), t.Format("15:04"))
}

func formatDateShort(t time.Time) string {
	return fmt.Sprintf("%d %s · %s",
		t.Day(), ukMonthsGenitive[t.Month()-1], t.Format("15:04"))
}

func formatClock(t time.Time) string { return t.Format("15:04") }

// escapeMarkdown escapes Telegram's "legacy" Markdown special chars for
// fields where we control the surrounding *…* / `…` formatting. We don't
// emit complex Markdown — escaping just _ * [ ` is enough.
func escapeMarkdown(s string) string {
	r := strings.NewReplacer(`_`, `\_`, `*`, `\*`, "`", "\\`", `[`, `\[`)
	return r.Replace(s)
}
