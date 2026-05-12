package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tele "gopkg.in/telebot.v3"
)

// Bot wires the Telegram bot to the store and ticket pipeline.
type Bot struct {
	tb       *tele.Bot
	store    *Store
	coder    *Coder
	show     Show
	jarLink  string        // shareable mono jar URL the user pays to
	hold     time.Duration // how long a reservation stays held before expiring
}

func NewBot(token string, store *Store, coder *Coder, show Show, jarLink string, hold time.Duration) (*Bot, error) {
	tb, err := tele.NewBot(tele.Settings{
		Token:  token,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	})
	if err != nil {
		return nil, err
	}
	b := &Bot{tb: tb, store: store, coder: coder, show: show, jarLink: jarLink, hold: hold}
	b.routes()
	return b, nil
}

func (b *Bot) Start() { b.tb.Start() }
func (b *Bot) Stop()  { b.tb.Stop() }

// SendTicket pushes a generated PDF back to the user who booked it.
// Called from the mono webhook OnEvent after a successful confirmation.
func (b *Bot) SendTicket(chatID int64, seat Seat, pdf []byte) error {
	recipient := tele.ChatID(chatID)
	doc := &tele.Document{
		File:     tele.FromReader(bytes.NewReader(pdf)),
		FileName: fmt.Sprintf("ticket-%d-%d.pdf", seat.Row, seat.Col),
		Caption:  fmt.Sprintf("Готово ✅\nРяд %d, місце %d", seat.Row, seat.Col),
	}
	_, err := b.tb.Send(recipient, doc)
	return err
}

func (b *Bot) routes() {
	b.tb.Handle("/start", b.handleStart)
	b.tb.Handle("/seats", b.handleSeats)
	b.tb.Handle("/my", b.handleMy)
	b.tb.Handle("/scan", b.handleScan)
	b.tb.Handle(tele.OnText, b.handleText)
	b.tb.Handle(tele.OnCallback, b.handleCallback)
}

func (b *Bot) handleStart(c tele.Context) error {
	return c.Send(fmt.Sprintf(
		"Вітаю! Це бот продажу квитків на %q (%s, %s).\n\n"+
			"Команди:\n"+
			"  /seats — мапа місць\n"+
			"  /my — мої бронювання\n"+
			"  /scan <код> — перевірити квиток (для контролерів)\n\n"+
			"Щоб купити: натисни /seats → обери вільне місце.",
		b.show.Title, b.show.Venue, b.show.StartsAt.Format("2 Jan 2006, 15:04")))
}

func (b *Bot) handleSeats(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	seats, err := b.store.Seats(ctx, b.show.ID)
	if err != nil {
		return c.Send("storage error: " + err.Error())
	}
	status, err := b.store.SeatStatus(ctx, b.show.ID)
	if err != nil {
		return c.Send("storage error: " + err.Error())
	}

	rows := make(map[int][]Seat)
	maxRow, maxCol := 0, 0
	for _, s := range seats {
		rows[s.Row] = append(rows[s.Row], s)
		if s.Row > maxRow {
			maxRow = s.Row
		}
		if s.Col > maxCol {
			maxCol = s.Col
		}
	}

	markup := &tele.ReplyMarkup{}
	keyboard := make([][]tele.InlineButton, 0, maxRow)
	for r := 1; r <= maxRow; r++ {
		row := make([]tele.InlineButton, 0, maxCol)
		for _, s := range rows[r] {
			label := fmt.Sprintf("%d", s.Col)
			switch status[s.ID] {
			case "sold":
				label = "✖"
			case "held":
				label = "…"
			}
			row = append(row, tele.InlineButton{
				Unique: "seat",
				Text:   label,
				Data:   fmt.Sprintf("%d:%d", s.Row, s.Col),
			})
		}
		keyboard = append(keyboard, row)
	}
	markup.InlineKeyboard = keyboard

	return c.Send(fmt.Sprintf("Ряд × Місце. Натисни кнопку, щоб забронювати.\nЦіна: %s",
		hryvnia(seats[0].PriceKopecks)), markup)
}

func (b *Bot) handleCallback(c tele.Context) error {
	cb := c.Callback()
	if cb == nil {
		return nil
	}
	// Telebot v3 prefixes callback data with the Unique value and a "|"
	// when InlineButton.Unique is set: "\fseat|3:5".
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

	code, err := b.coder.NewReservationCode()
	if err != nil {
		return c.Respond(&tele.CallbackResponse{Text: "internal error"})
	}
	r, err := b.store.Reserve(ctx, seat, cb.Sender.ID, cb.Message.Chat.ID, code, b.hold)
	if err != nil {
		return c.Respond(&tele.CallbackResponse{Text: friendly(err)})
	}

	_ = c.Respond(&tele.CallbackResponse{Text: "Місце притримано"})
	_, err = b.tb.Send(cb.Sender, fmt.Sprintf(
		"Місце забронювано: ряд %d, місце %d.\n\n"+
			"Сплати %s на банку: %s\n"+
			"У КОМЕНТАРІ обовʼязково вкажи код:\n\n"+
			"`%s`\n\n"+
			"Бронювання дійсне до %s. Після оплати бот сам пришле PDF.",
		seat.Row, seat.Col, hryvnia(seat.PriceKopecks), b.jarLink, r.Code,
		r.ExpiresAt.Format("15:04")), tele.ModeMarkdown)
	return err
}

func (b *Bot) handleMy(c tele.Context) error {
	return c.Send("Скоро тут буде список ваших бронювань.")
}

func (b *Bot) handleScan(c tele.Context) error {
	args := strings.TrimSpace(strings.TrimPrefix(c.Text(), "/scan"))
	if args == "" {
		return c.Send("Usage: /scan <QR-payload>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resID, seatID, err := b.coder.VerifyQRPayload(args)
	if err != nil {
		return c.Send("❌ INVALID: " + err.Error())
	}
	t, err := b.store.UseTicket(ctx, args)
	switch {
	case errors.Is(err, ErrTicketUsed):
		return c.Send(fmt.Sprintf("❌ ALREADY USED at %s", t.UsedAt.Format(time.RFC3339)))
	case errors.Is(err, ErrTicketNotFound):
		return c.Send("❌ NOT FOUND")
	case err != nil:
		return c.Send("⚠️ " + err.Error())
	}
	return c.Send(fmt.Sprintf("✅ OK — reservation %d, seat %d", resID, seatID))
}

func (b *Bot) handleText(c tele.Context) error {
	// Silent — main entrypoint is the inline keyboard.
	return nil
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
