// Package bot is the Telegram side of the ticket app.
//
// UX model (PR #5f1):
//   - /start без deep-link: показуємо афішу — inline keyboard з усіма
//     актуальними подіями.
//   - Тап події → меню події з кнопками "📍 Обрати місце" (WebApp →
//     /event/<slug>), "🎟 Мої бронювання", "↩ До списку".
//   - /start res_<code>: deep link з public web-flow; прив'язує цей
//     чат до резервації, щоб PDF прийшов сюди після оплати.
//
// Map-pick через inline kb (старий 5×6 grid) прибрано — мапа місць
// тепер живе у Telegram Mini App (/event/<slug>). Це уніфікує UX
// з вебом і знімає обмеження на ~8 кнопок у рядок.
package bot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/OlexiyOdarchuk/go-monobank-sdk/money"
	tele "gopkg.in/telebot.v3"
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
	CancelReservation(ctx context.Context, code string, tgUserID int64) (Reservation, Seat, error)
	MyReservations(ctx context.Context, tgUserID int64) ([]MyItem, error)
	Stats(ctx context.Context, showID int64) (Stats, error)
	// LinkReservationToTGChat attaches a Telegram chat to a web-buyer
	// reservation by its public code so the bot can deliver the PDF after
	// payment. Returns ErrCodeNotFound for unknown codes, ErrAlreadyClosed
	// for cancelled reservations.
	LinkReservationToTGChat(ctx context.Context, code string, tgUserID, tgChatID int64) (Reservation, Seat, error)
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
	reconciler Reconciler // optional — nil if MONO_TOKEN missing
	jar        JarLookup  // optional — nil if jar link unparseable
	showFn     ShowFn

	// pending tracks chat users mid-name-input after picking a seat
	// inline. Key = tg user id; value = pendingPick. A periodic sweep
	// drops entries past Until so abandoned picks don't pile up.
	pending sync.Map
	done    chan struct{}
}

type pendingPick struct {
	ShowID int64
	Row    int
	Col    int
	Until  time.Time
}

type Options struct {
	Token      string
	Store      Store
	Coder      Coder
	ShowFn     ShowFn
	BaseURL    string        // optional; without it WebApp buttons fall back to plain URL share
	JarLink    string
	Hold       time.Duration // how long a pre-paid hold lives
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
		tb: tb, store: opts.Store, coder: opts.Coder, showFn: opts.ShowFn,
		baseURL: strings.TrimRight(opts.BaseURL, "/"),
		jarLink: opts.JarLink, hold: opts.Hold, adminTGID: opts.AdminTGID,
		reconciler: opts.Reconciler, jar: opts.Jar,
		done: make(chan struct{}),
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
	case strings.HasPrefix(cb.Data, "\fcancel|"):
		return b.callbackCancel(c, cb)
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
	if b.baseURL != "" {
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
// held are "…", sold are "✖".
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
		return c.Respond(&tele.CallbackResponse{Text: "storage error"})
	}
	status, err := b.store.SeatStatuses(ctx, sh.ID)
	if err != nil {
		return c.Respond(&tele.CallbackResponse{Text: "storage error"})
	}
	_ = c.Respond(&tele.CallbackResponse{})

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
	if maxRow == 0 {
		return c.Send("Зала ще не налаштована. Спробуй пізніше.")
	}

	kb := make([][]tele.InlineButton, 0, maxRow+1)
	for r := 1; r <= maxRow; r++ {
		rowSeats := rowMap[r]
		btns := make([]tele.InlineButton, 0, len(rowSeats))
		for _, s := range rowSeats {
			label := strconv.Itoa(s.Col)
			switch status[s.ID] {
			case SeatSold:
				label = "✖"
			case SeatHeld:
				label = "…"
			}
			btns = append(btns, tele.InlineButton{
				Unique: "seat",
				Text:   label,
				Data:   fmt.Sprintf("%s:%d:%d", sh.Slug, s.Row, s.Col),
			})
		}
		kb = append(kb, btns)
	}
	kb = append(kb, []tele.InlineButton{
		{Unique: "show", Text: "↩ Назад", Data: sh.Slug},
	})
	markup := &tele.ReplyMarkup{InlineKeyboard: kb}

	priceLine := ""
	if len(seats) > 0 {
		priceLine = "\nЦіна вказана у редакторі залу адміном."
	}
	header := "🎭 ━━━━━ СЦЕНА ━━━━━ 🎭\n               ▲ ближче до сцени\n\n"
	return c.Send(
		fmt.Sprintf("%sРяд 1 — найближче до сцени.\nНатисни вільне місце, щоб забронювати.%s",
			header, priceLine),
		markup)
}

// callbackSeat handles a tap on one of the seat buttons. Stores the
// (showID, row, col) tuple in pending, asks for the buyer's name with
// ForceReply, and waits for handleText to pick it up.
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
	// Pre-check that the seat is still free so the name prompt isn't a
	// dead end. Re-checked atomically inside Reserve.
	seat, err := b.store.FindFreeSeat(ctx, sh.ID, row, col)
	if err != nil {
		return c.Respond(&tele.CallbackResponse{Text: friendly(err)})
	}
	b.pending.Store(cb.Sender.ID, pendingPick{
		ShowID: sh.ID, Row: row, Col: col,
		Until: time.Now().Add(pendingTTL),
	})
	_ = c.Respond(&tele.CallbackResponse{Text: "Введи ім'я"})
	_, err = b.tb.Send(cb.Sender, fmt.Sprintf(
		"Місце ряд %d · %d, ціна %s.\nВведи ім'я та прізвище — будуть надруковані на квитку:",
		seat.Row, seat.Col, seat.Price),
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
		return nil // user typed text but isn't in name-input mode
	}
	pick, ok := raw.(pendingPick)
	if !ok {
		b.pending.Delete(sender.ID)
		return nil
	}
	if time.Now().After(pick.Until) {
		b.pending.Delete(sender.ID)
		return c.Send("Час на введення імені вийшов. Тицьни /events і вибери місце ще раз.")
	}

	name, err := normalizeName(c.Text())
	if err != nil {
		return c.Send(err.Error() + "\nСпробуй ще раз:")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	seat, err := b.store.FindFreeSeat(ctx, pick.ShowID, pick.Row, pick.Col)
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

	payURL := jarPrefillURL(b.jarLink, seat.Price, r.Code)
	payBtn := tele.InlineButton{Text: "💳 Оплатити", URL: payURL}
	cancelBtn := tele.InlineButton{Unique: "cancel", Text: "✖ Скасувати бронь", Data: r.Code}
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

	r, seat, err := b.store.LinkReservationToTGChat(ctx, code, sender.ID, c.Chat().ID)
	switch {
	case errors.Is(err, ErrCodeNotFound):
		return c.Send("Цю бронь не знайдено. Можливо, посилання застаріле.")
	case errors.Is(err, ErrAlreadyClosed):
		return c.Send("Цю бронь вже скасовано.")
	case err != nil:
		slog.Error("link reservation", "code", code, "err", err)
		return c.Send("Внутрішня помилка, спробуй пізніше.")
	}

	if r.ConfirmedAt != nil {
		return c.Send(fmt.Sprintf(
			"Бронь вже оплачена 🎉\nРяд %d, місце %d.\nКвиток із QR прийшов на email — перевір пошту.",
			seat.Row, seat.Col))
	}
	return c.Send(fmt.Sprintf(
		"Підключив цю бронь до Telegram ✅\nРяд %d, місце %d.\nПісля оплати квиток із QR прийде сюди (і на email також).",
		seat.Row, seat.Col))
}

// --- /my ---

func (b *Bot) handleMy(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	items, err := b.store.MyReservations(ctx, c.Sender().ID)
	if err != nil {
		return c.Send("storage error: " + err.Error())
	}
	if len(items) == 0 {
		return c.Send("У тебе ще немає бронювань. /events — афіша.")
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
