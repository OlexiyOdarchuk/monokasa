// Package pay glues the mono webhook to the rest of the app: match a
// transfer against an open reservation by code, confirm it, render the
// ticket PDF and send it to the buyer.
package pay

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/vtopc/go-monobank"

	"github.com/OlexiyOdarchuk/mono-tix/internal/bot"
	"github.com/OlexiyOdarchuk/mono-tix/internal/store"
	"github.com/OlexiyOdarchuk/mono-tix/internal/ticket"
	"github.com/OlexiyOdarchuk/mono-tix/internal/token"
)

type Processor struct {
	Store        *store.Store
	Coder        *token.Coder
	Bot          *bot.Bot
	Show         store.Show
	PriceKopecks int64
}

// Handle is the OnEvent callback wired into monobank.WebhookHandler.
func (p *Processor) Handle(ctx context.Context, e *monobank.WebHookResponse) error {
	t := e.Data.Transaction
	if t.Amount <= 0 {
		return nil // outflow, not interesting
	}

	code := extractCode(t.Comment, t.Description)
	if code == "" {
		log.Printf("payment %s: no reservation code in comment %q", t.ID, t.Comment)
		return nil
	}
	res, seat, err := p.Store.FindReservationByCode(ctx, code)
	if errors.Is(err, store.ErrCodeNotFound) || errors.Is(err, store.ErrAlreadyClosed) {
		log.Printf("payment %s: code %q has no open reservation", t.ID, code)
		return nil
	}
	if err != nil {
		return err
	}
	if res.ConfirmedAt != nil {
		log.Printf("payment %s: reservation %s already confirmed", t.ID, code)
		return nil
	}
	if t.Amount < p.PriceKopecks {
		log.Printf("payment %s: short amount %d (need %d) for code %s",
			t.ID, t.Amount, p.PriceKopecks, code)
		return nil
	}

	qrPayload := p.Coder.QRPayload(res.ID, seat.ID)
	if _, err := p.Store.Confirm(ctx, res.ID, qrPayload); err != nil {
		return err
	}
	pdf, err := ticket.RenderPDF(p.Show, seat, res.BuyerName, qrPayload)
	if err != nil {
		return err
	}
	if err := p.Bot.SendTicket(res.TGChatID, seat, pdf); err != nil {
		return err
	}
	log.Printf("ticket issued: code=%s row=%d seat=%d buyer=%q chat=%d",
		code, seat.Row, seat.Col, res.BuyerName, res.TGChatID)
	return nil
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
