// Command mono_tix is a one-evening MVP ticket-selling bot driven by mono
// jar webhooks. Telegram bot lets users pick a seat and gives them a unique
// payment comment; when mono confirms a matching transfer, the webhook
// handler issues a signed PDF ticket with a QR code and pushes it back to
// the buyer's Telegram chat. /scan validates a QR code at the entrance.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/vtopc/go-monobank"
)

func main() {
	_ = godotenv.Load()

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	showID, err := store.SeedIfEmpty(ctx, Show{
		Title:    cfg.ShowTitle,
		Venue:    cfg.ShowVenue,
		StartsAt: cfg.ShowStartsAt,
	}, cfg.Rows, cfg.Cols, cfg.PriceKopecks)
	if err != nil {
		log.Fatalf("seed: %v", err)
	}
	show := Show{
		ID:       showID,
		Title:    cfg.ShowTitle,
		Venue:    cfg.ShowVenue,
		StartsAt: cfg.ShowStartsAt,
	}
	log.Printf("show #%d ready: %q, %d×%d seats @ %s",
		showID, show.Title, cfg.Rows, cfg.Cols, hryvnia(cfg.PriceKopecks))

	coder := NewCoder(cfg.Secret)

	bot, err := NewBot(cfg.TGToken, store, coder, show, cfg.JarLink, cfg.HoldDuration)
	if err != nil {
		log.Fatalf("bot init: %v", err)
	}
	go bot.Start()
	defer bot.Stop()
	log.Printf("telegram bot up")

	monoClient := monobank.NewClient(nil)
	handler, err := monobank.NewWebhookHandler(ctx, monobank.WebhookHandlerOptions{
		Keys:  monoClient,
		Dedup: monobank.NewMemoryDeduper(2048),
		OnEvent: func(_ context.Context, e *monobank.WebHookResponse) error {
			return handlePayment(ctx, store, coder, bot, show, cfg.PriceKopecks, e)
		},
		OnError: func(err error) { log.Printf("webhook: %v", err) },
	})
	if err != nil {
		log.Fatalf("webhook handler init: %v", err)
	}
	log.Printf("mono webhook handler ready, keyId=%s", handler.KeyID())

	scanner := NewScanServer(store, coder, cfg.ScannerToken)
	mux := http.NewServeMux()
	mux.Handle("/webhook", handler)
	scanner.Register(mux)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	if cfg.ScannerToken == "" {
		log.Printf("WARN: SCANNER_TOKEN порожній — /scan відкритий для всіх (ОК для локального тесту)")
	}

	log.Printf("listening on %s", cfg.HTTPAddr)
	log.Fatal(http.ListenAndServe(cfg.HTTPAddr, mux))
}

// handlePayment matches an incoming mono transfer against an open
// reservation by its code (parsed from the Comment field) and the exact
// amount, then confirms the reservation, generates a PDF and pushes it to
// the buyer's Telegram chat.
func handlePayment(
	ctx context.Context,
	store *Store, coder *Coder, bot *Bot,
	show Show, priceKopecks int64,
	e *monobank.WebHookResponse,
) error {
	t := e.Data.Transaction
	// Only credits — outflows from the jar (somebody withdrawing) are noise.
	if t.Amount <= 0 {
		return nil
	}

	code := extractCode(t.Comment, t.Description)
	if code == "" {
		log.Printf("payment %s: no reservation code in comment %q", t.ID, t.Comment)
		return nil
	}

	res, seat, err := store.FindReservationByCode(ctx, code)
	if errors.Is(err, ErrCodeNotFound) {
		log.Printf("payment %s: code %q not found", t.ID, code)
		return nil
	}
	if err != nil {
		return err
	}
	if res.ConfirmedAt != nil {
		log.Printf("payment %s: reservation %s already confirmed", t.ID, code)
		return nil
	}
	if t.Amount < priceKopecks {
		log.Printf("payment %s: short amount %d (need %d) for code %s",
			t.ID, t.Amount, priceKopecks, code)
		return nil
	}

	qrPayload := coder.QRPayload(res.ID, seat.ID)
	if _, err := store.Confirm(ctx, res.ID, qrPayload); err != nil {
		return err
	}

	pdf, err := RenderTicketPDF(show, seat, qrPayload)
	if err != nil {
		return err
	}
	if err := bot.SendTicket(res.TGChatID, seat, pdf); err != nil {
		return err
	}
	log.Printf("ticket issued: code=%s row=%d seat=%d to chat=%d",
		code, seat.Row, seat.Col, res.TGChatID)
	return nil
}

// extractCode pulls the reservation code out of the user-supplied comment.
// Mono's `comment` field carries whatever the sender typed; we accept both
// the plain code and "Comment with code abc12345 in the middle".
func extractCode(fields ...string) string {
	for _, f := range fields {
		f = strings.ToLower(strings.TrimSpace(f))
		// Reservation codes are 8 base32 chars (NewReservationCode).
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

// --- config ---

type Config struct {
	TGToken       string
	Secret        string
	JarLink       string
	ScannerToken  string
	HTTPAddr      string
	DBPath        string
	ShowTitle     string
	ShowVenue     string
	ShowStartsAt  time.Time
	Rows, Cols    int
	PriceKopecks  int64
	HoldDuration  time.Duration
}

func loadConfig() (Config, error) {
	c := Config{
		HTTPAddr:     getenv("HTTP_ADDR", ":8090"),
		DBPath:       getenv("DB_PATH", "tix.db"),
		ShowTitle:    getenv("SHOW_TITLE", "Моя вистава"),
		ShowVenue:    getenv("SHOW_VENUE", "Театральна площа"),
		Rows:         getenvInt("ROWS", 5),
		Cols:         getenvInt("COLS", 6),
		PriceKopecks: int64(getenvInt("PRICE_KOPECKS", 25000)), // 250.00 UAH
		HoldDuration: getenvDur("HOLD", 15*time.Minute),
	}
	c.TGToken = os.Getenv("TG_TOKEN")
	c.Secret = os.Getenv("TICKET_SECRET")
	c.JarLink = os.Getenv("MONO_JAR_LINK")
	c.ScannerToken = os.Getenv("SCANNER_TOKEN")
	if c.TGToken == "" {
		return c, errors.New("TG_TOKEN is required")
	}
	if c.Secret == "" {
		return c, errors.New("TICKET_SECRET is required (HMAC key for QR/codes)")
	}
	if c.JarLink == "" {
		return c, errors.New("MONO_JAR_LINK is required (the send.monobank.ua/... URL of the jar)")
	}

	t, err := time.Parse(time.RFC3339, getenv("SHOW_STARTS_AT", "2026-06-01T19:00:00+03:00"))
	if err != nil {
		return c, err
	}
	c.ShowStartsAt = t
	return c, nil
}

func getenv(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func getenvInt(k string, fallback int) int {
	if v := os.Getenv(k); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return fallback
}

func getenvDur(k string, fallback time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
