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
	"time"
	"unicode/utf8"

	"github.com/OlexiyOdarchuk/go-monobank-sdk/money"
	tele "gopkg.in/telebot.v3"
)

// Show is the subset of show info the bot needs. Slug is used to build
// the WebApp / public-link URLs the bot ships to buyers.
type Show struct {
	ID       int64
	Slug     string
	Title    string
	Venue    string
	StartsAt time.Time
}

// Seat is the subset of seat info the bot needs (for reservation cards,
// reminders, ticket captions). The map-pick UI doesn't live in the bot
// any more — only delivery and admin commands surface seat info.
type Seat struct {
	ID       int64
	ShowID   int64
	Row, Col int
	Price    money.Money
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
	ErrCodeNotFound   = errors.New("reservation code not found")
	ErrAlreadyPaid    = errors.New("reservation already confirmed")
	ErrAlreadyClosed  = errors.New("reservation already closed")
	ErrNotYourBooking = errors.New("reservation belongs to another user")
	ErrShowNotFound   = errors.New("show not found")
)

// Store is the persistence behavior the bot needs.
type Store interface {
	// Shows lists upcoming, non-archived shows — used to render the
	// "афіша" menu when the user runs /start without a deep link.
	Shows(ctx context.Context) ([]Show, error)
	// FindShowBySlug resolves a slug from a "show:<slug>" callback into
	// the full Show record.
	FindShowBySlug(ctx context.Context, slug string) (Show, error)
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
	baseURL    string // e.g. https://monokasa.app — used for WebApp deep links
	jarLink    string
	adminTGID  int64
	reconciler Reconciler // optional — nil if MONO_TOKEN missing
	jar        JarLookup  // optional — nil if jar link unparseable
	showFn     ShowFn

	done chan struct{}
}

type Options struct {
	Token      string
	Store      Store
	ShowFn     ShowFn
	BaseURL    string // optional; without it WebApp buttons fall back to plain URL share
	JarLink    string
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
		tb: tb, store: opts.Store, showFn: opts.ShowFn,
		baseURL: strings.TrimRight(opts.BaseURL, "/"),
		jarLink: opts.JarLink, adminTGID: opts.AdminTGID,
		reconciler: opts.Reconciler, jar: opts.Jar,
		done: make(chan struct{}),
	}
	b.routes()
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

	markup := &tele.ReplyMarkup{}
	pickRow := []tele.InlineButton{}
	if b.baseURL != "" {
		pickRow = append(pickRow, tele.InlineButton{
			Text:   "📍 Обрати місце",
			WebApp: &tele.WebApp{URL: fmt.Sprintf("%s/event/%s", b.baseURL, sh.Slug)},
		})
	}
	markup.InlineKeyboard = [][]tele.InlineButton{
		pickRow,
		{{Unique: "events", Text: "↩ До списку", Data: "back"}},
	}
	if len(pickRow) == 0 {
		// No BASE_URL — share the URL anyway so the user can open it
		// manually in a browser. Better than a dead-end card.
		markup.InlineKeyboard = [][]tele.InlineButton{
			{{Unique: "events", Text: "↩ До списку", Data: "back"}},
		}
	}

	text := fmt.Sprintf("🎭 *%s*\n📅 %s\n",
		escapeMarkdown(sh.Title), formatDateTime(sh.StartsAt))
	if sh.Venue != "" {
		text += "📍 " + escapeMarkdown(sh.Venue) + "\n"
	}
	if b.baseURL == "" {
		text += fmt.Sprintf("\nЩоб обрати місце — відкрий у браузері:\nmonokasa/event/%s",
			escapeMarkdown(sh.Slug))
	} else {
		text += "\nТицяй *📍 Обрати місце* — відкриється мапа залу."
	}
	return c.Send(text, tele.ModeMarkdown, markup)
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
