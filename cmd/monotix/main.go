// Command monotix is the entry point of the ticket-selling bot.
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
	"github.com/vtopc/go-monobank"

	"github.com/OlexiyOdarchuk/mono-tix/internal/bot"
	"github.com/OlexiyOdarchuk/mono-tix/internal/config"
	"github.com/OlexiyOdarchuk/mono-tix/internal/pay"
	"github.com/OlexiyOdarchuk/mono-tix/internal/store"
	"github.com/OlexiyOdarchuk/mono-tix/internal/timefmt"
	"github.com/OlexiyOdarchuk/mono-tix/internal/token"
	"github.com/OlexiyOdarchuk/mono-tix/internal/web"
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
		Store:     st,
		Coder:     coder,
		Show:      show,
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

	monoClient := monobank.NewClient(nil)
	processor := &pay.Processor{
		Store: st, Coder: coder, Bot: tg, Show: show, PriceKopecks: cfg.PriceKopecks,
	}
	hook, err := monobank.NewWebhookHandler(ctx, monobank.WebhookHandlerOptions{
		Keys:    monoClient,
		Dedup:   monobank.NewMemoryDeduper(2048),
		OnEvent: processor.Handle,
		OnError: func(err error) { log.Printf("webhook: %v", err) },
	})
	if err != nil {
		log.Fatalf("webhook handler init: %v", err)
	}
	log.Printf("mono webhook ready, keyId=%s", hook.KeyID())

	scanner := web.NewScanner(st, coder, cfg.ScannerToken)

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
			if err := tg.NotifyShowSoon(it.Reservation.TGChatID, it.Seat, show.StartsAt); err != nil {
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
