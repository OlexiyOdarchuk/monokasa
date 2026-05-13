// Command monokasa is the entry point of the ticket-selling bot.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/OlexiyOdarchuk/go-monobank-sdk/bank"
	"github.com/OlexiyOdarchuk/go-monobank-sdk/webhook"

	"github.com/OlexiyOdarchuk/monokasa/internal/bot"
	"github.com/OlexiyOdarchuk/monokasa/internal/config"
	"github.com/OlexiyOdarchuk/monokasa/internal/pay"
	"github.com/OlexiyOdarchuk/monokasa/internal/store"
	"github.com/OlexiyOdarchuk/monokasa/internal/ticket"
	"github.com/OlexiyOdarchuk/monokasa/internal/timefmt"
	"github.com/OlexiyOdarchuk/monokasa/internal/token"
	"github.com/OlexiyOdarchuk/monokasa/internal/web"
)

func main() {
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	ctx, cancel := signalCtx()
	defer cancel()

	showID, err := st.SeedIfEmpty(ctx, store.Show{
		Title:    cfg.ShowTitle,
		Venue:    cfg.ShowVenue,
		StartsAt: cfg.ShowStartsAt,
	}, cfg.Rows, cfg.Cols, cfg.PriceKopecks)
	if err != nil {
		log.Fatalf("seed: %v", err)
	}
	show, err := st.LoadShow(ctx, showID)
	if err != nil {
		log.Fatalf("load show: %v", err)
	}
	log.Printf("show #%d ready: %q @ %s", show.ID, show.Title, timefmt.DateTime(show.StartsAt))

	coder := token.NewCoder(cfg.Secret)

	tg, err := bot.New(bot.Options{
		Token:     cfg.TGToken,
		Store:     botStore{st},
		Coder:     coder,
		Show:      bot.Show(show),
		JarLink:   cfg.JarLink,
		Hold:      cfg.HoldDuration,
		AdminTGID: cfg.AdminTGID,
	})
	if err != nil {
		log.Fatalf("bot init: %v", err)
	}
	go tg.Start()
	defer tg.Stop()
	log.Printf("telegram bot up")

	monoClient := bank.New()
	processor := &pay.Processor{
		Store:        payStore{st},
		Coder:        coder,
		Notifier:     payNotifier{tg},
		Renderer:     payRenderer,
		Show:         pay.Show{Title: show.Title, Venue: show.Venue, StartsAt: show.StartsAt},
		PriceKopecks: cfg.PriceKopecks,
	}
	hook, err := webhook.NewHandler(ctx, webhook.Options{
		Keys:    monoClient,
		Dedup:   webhook.NewMemoryDeduper(2048),
		OnEvent: processor.Handle,
		OnError: func(err error) { log.Printf("webhook: %v", err) },
	})
	if err != nil {
		log.Fatalf("webhook handler init: %v", err)
	}
	log.Printf("mono webhook ready, keyId=%s", hook.KeyID())

	scanner := web.NewScanner(webStore{st}, coder, cfg.ScannerToken)

	mux := http.NewServeMux()
	mux.Handle("/webhook", hook)
	scanner.Register(mux)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: mux}
	go func() {
		log.Printf("listening on %s", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()
	defer func() {
		shutCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = srv.Shutdown(shutCtx)
	}()

	// Reminders.
	go runReminderLoop(ctx, st, tg, show, cfg.RemindBefore)

	<-ctx.Done()
	log.Printf("shutting down")
}

func signalCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}

// runReminderLoop wakes up periodically; when we're within
// `remindBefore` of show start, it pings every paid-and-not-yet-reminded
// reservation once, then idles. The DB column reminded_at guarantees
// at-most-once delivery across restarts.
func runReminderLoop(ctx context.Context, st *store.Store, tg *bot.Bot, show store.Show, remindBefore time.Duration) {
	tick := time.NewTicker(1 * time.Minute)
	defer tick.Stop()
	check := func() {
		until := time.Until(show.StartsAt)
		if until > remindBefore || until < -2*time.Hour {
			return // too early or already long over
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		items, err := st.ConfirmedNotYetReminded(ctx, show.ID)
		if err != nil {
			log.Printf("remind: %v", err)
			return
		}
		for _, it := range items {
			if err := tg.NotifyShowSoon(it.Reservation.TGChatID, bot.Seat(it.Seat), show.StartsAt); err != nil {
				log.Printf("remind chat=%d: %v", it.Reservation.TGChatID, err)
				continue
			}
			if err := st.MarkReminded(ctx, it.Reservation.ID); err != nil {
				log.Printf("mark reminded id=%d: %v", it.Reservation.ID, err)
			}
		}
	}
	check() // fire once at startup so a long-running process catches up
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			check()
		}
	}
}

// --- bot adapter ---

// botStore wraps *store.Store to satisfy bot.Store: store types and bot types
// are field-compatible, so a single struct conversion does the translation.
type botStore struct{ s *store.Store }

func (b botStore) Seats(ctx context.Context, showID int64) ([]bot.Seat, error) {
	seats, err := b.s.Seats(ctx, showID)
	if err != nil {
		return nil, err
	}
	out := make([]bot.Seat, len(seats))
	for i, s := range seats {
		out[i] = bot.Seat(s)
	}
	return out, nil
}

func (b botStore) SeatStatuses(ctx context.Context, showID int64) (map[int64]bot.SeatStatus, error) {
	in, err := b.s.SeatStatuses(ctx, showID)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bot.SeatStatus, len(in))
	for id, st := range in {
		out[id] = bot.SeatStatus(st)
	}
	return out, nil
}

func (b botStore) FindFreeSeat(ctx context.Context, showID int64, row, col int) (bot.Seat, error) {
	s, err := b.s.FindFreeSeat(ctx, showID, row, col)
	return bot.Seat(s), translateStoreErr(err)
}

func (b botStore) Reserve(
	ctx context.Context, seat bot.Seat, tgUserID, tgChatID int64,
	buyerName, code string, hold time.Duration,
) (bot.Reservation, error) {
	r, err := b.s.Reserve(ctx, store.Seat(seat), tgUserID, tgChatID, buyerName, code, hold)
	return bot.Reservation(r), translateStoreErr(err)
}

func (b botStore) CancelReservation(ctx context.Context, code string, tgUserID int64) (bot.Reservation, bot.Seat, error) {
	r, s, err := b.s.CancelReservation(ctx, code, tgUserID)
	return bot.Reservation(r), bot.Seat(s), translateStoreErr(err)
}

func (b botStore) MyReservations(ctx context.Context, tgUserID int64) ([]bot.MyItem, error) {
	items, err := b.s.MyReservations(ctx, tgUserID)
	if err != nil {
		return nil, err
	}
	out := make([]bot.MyItem, len(items))
	for i, it := range items {
		out[i] = bot.MyItem{Reservation: bot.Reservation(it.Reservation), Seat: bot.Seat(it.Seat)}
	}
	return out, nil
}

func (b botStore) Stats(ctx context.Context, showID int64) (bot.Stats, error) {
	s, err := b.s.Stats(ctx, showID)
	return bot.Stats(s), err
}

func translateStoreErr(err error) error {
	switch err {
	case nil:
		return nil
	case store.ErrSeatTaken:
		return bot.ErrSeatTaken
	case store.ErrSeatNotFound:
		return bot.ErrSeatNotFound
	case store.ErrCodeNotFound:
		return bot.ErrCodeNotFound
	case store.ErrAlreadyPaid:
		return bot.ErrAlreadyPaid
	case store.ErrAlreadyClosed:
		return bot.ErrAlreadyClosed
	case store.ErrNotYourBooking:
		return bot.ErrNotYourBooking
	default:
		return err
	}
}

// --- web adapter ---

type webStore struct{ s *store.Store }

func (w webStore) UseTicket(ctx context.Context, qrPayload string) (web.Ticket, error) {
	t, err := w.s.UseTicket(ctx, qrPayload)
	out := web.Ticket{ID: t.ID, UsedAt: t.UsedAt}
	switch err {
	case nil:
		return out, nil
	case store.ErrTicketNotFound:
		return out, web.ErrTicketNotFound
	case store.ErrTicketUsed:
		return out, web.ErrTicketUsed
	default:
		return out, err
	}
}

func (w webStore) FindReservationByTicket(ctx context.Context, ticketID int64) (web.Reservation, web.Seat, error) {
	r, s, err := w.s.FindReservationByTicket(ctx, ticketID)
	return web.Reservation{BuyerName: r.BuyerName, ConfirmedAt: r.ConfirmedAt},
		web.Seat{ID: s.ID, Row: s.Row, Col: s.Col},
		err
}

// --- pay adapter ---

type payStore struct{ s *store.Store }

func (p payStore) FindReservationByCode(ctx context.Context, code string) (pay.Reservation, pay.Seat, error) {
	r, s, err := p.s.FindReservationByCode(ctx, code)
	out := pay.Reservation{
		ID:          r.ID,
		TGChatID:    r.TGChatID,
		BuyerName:   r.BuyerName,
		ConfirmedAt: r.ConfirmedAt,
	}
	seat := pay.Seat{ID: s.ID, Row: s.Row, Col: s.Col, PriceKopecks: s.PriceKopecks}
	switch err {
	case nil:
		return out, seat, nil
	case store.ErrCodeNotFound:
		return out, seat, pay.ErrCodeNotFound
	case store.ErrAlreadyClosed:
		return out, seat, pay.ErrAlreadyClosed
	default:
		return out, seat, err
	}
}

func (p payStore) Confirm(ctx context.Context, reservationID int64, qrPayload string) error {
	_, err := p.s.Confirm(ctx, reservationID, qrPayload)
	return err
}

type payNotifier struct{ b *bot.Bot }

func (p payNotifier) SendTicket(chatID int64, seat pay.Seat, pdf []byte) error {
	return p.b.SendTicket(chatID, bot.Seat{
		ID: seat.ID, Row: seat.Row, Col: seat.Col, PriceKopecks: seat.PriceKopecks,
	}, pdf)
}

func payRenderer(show pay.Show, seat pay.Seat, buyerName, qrPayload string) ([]byte, error) {
	return ticket.RenderPDF(
		ticket.Show{Title: show.Title, Venue: show.Venue, StartsAt: show.StartsAt},
		ticket.Seat{Row: seat.Row, Col: seat.Col},
		buyerName, qrPayload,
	)
}
