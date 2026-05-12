// Package bot is the Telegram side of the ticket app: seat picking,
// payment instructions, cancellation, /my, /stats.
package bot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

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

type Bot struct {
	tb        *tele.Bot
	store     Store
	coder     Coder
	show      Show
	jarLink   string
	hold      time.Duration
	adminTGID int64
}

type Options struct {
	Token     string
	Store     Store
	Coder     Coder
	Show      Show
	JarLink   string
	Hold      time.Duration
	AdminTGID int64
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
	}
	b.routes()
	return b, nil
}

func (b *Bot) Start() { b.tb.Start() }
func (b *Bot) Stop()  { b.tb.Stop() }

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
	b.tb.Handle(tele.OnText, b.handleText)
	b.tb.Handle(tele.OnCallback, b.handleCallback)
}

func (b *Bot) handleStart(c tele.Context) error {
	return c.Send(fmt.Sprintf(
		"Вітаю! Це бот продажу квитків на %q (%s, %s).\n\n"+
			"Команди:\n"+
			"  /seats — мапа місць\n"+
			"  /my — мої бронювання\n\n"+
			"Щоб купити: натисни /seats → обери вільне місце.",
		b.show.Title, b.show.Venue, formatDateTime(b.show.StartsAt)))
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

	seat, err := b.store.FindFreeSeat(ctx, b.show.ID, row, col)
	if err != nil {
		return c.Respond(&tele.CallbackResponse{Text: friendly(err)})
	}

	code, err := b.coder.NewCode()
	if err != nil {
		return c.Respond(&tele.CallbackResponse{Text: "internal error"})
	}
	buyer := buyerName(cb.Sender)
	r, err := b.store.Reserve(ctx, seat, cb.Sender.ID, cb.Message.Chat.ID, buyer, code, b.hold)
	if err != nil {
		return c.Respond(&tele.CallbackResponse{Text: friendly(err)})
	}

	_ = c.Respond(&tele.CallbackResponse{Text: "Місце притримано"})
	payURL := jarPrefillURL(b.jarLink, seat.PriceKopecks, r.Code)

	cancelBtn := tele.InlineButton{Unique: "cancel", Text: "✖ Скасувати бронь", Data: r.Code}
	markup := &tele.ReplyMarkup{InlineKeyboard: [][]tele.InlineButton{{cancelBtn}}}

	_, err = b.tb.Send(cb.Sender, fmt.Sprintf(
		"Місце забронювано: ряд %d, місце %d.\n\n"+
			"💳 Оплата (сума й коментар вже вписані):\n%s\n\n"+
			"Код у коментарі — `%s` (моно зробить це поле read-only).\n"+
			"Бронювання дійсне до %s. Після оплати бот сам пришле PDF.",
		seat.Row, seat.Col, payURL, r.Code, formatClock(r.ExpiresAt)),
		tele.ModeMarkdown,
		&tele.SendOptions{DisableWebPagePreview: true},
		markup)
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

func (b *Bot) handleText(c tele.Context) error { return nil }

func buyerName(u *tele.User) string {
	if u == nil {
		return ""
	}
	if u.FirstName != "" && u.LastName != "" {
		return u.FirstName + " " + u.LastName
	}
	if u.FirstName != "" {
		return u.FirstName
	}
	if u.Username != "" {
		return "@" + u.Username
	}
	return ""
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
