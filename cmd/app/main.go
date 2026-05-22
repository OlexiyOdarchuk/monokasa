// Command monokasa is the entry point of the ticket-selling bot.
package main

import (
	"context"
	"errors"
	"expvar"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	monobank "github.com/OlexiyOdarchuk/go-monobank-sdk"
	"github.com/OlexiyOdarchuk/go-monobank-sdk/bank"
	"github.com/OlexiyOdarchuk/go-monobank-sdk/currency"
	"github.com/OlexiyOdarchuk/go-monobank-sdk/jar"
	"github.com/OlexiyOdarchuk/go-monobank-sdk/money"
	"github.com/OlexiyOdarchuk/go-monobank-sdk/personal"
	"github.com/OlexiyOdarchuk/go-monobank-sdk/webhook"

	"github.com/OlexiyOdarchuk/monokasa/internal/admin"
	"github.com/OlexiyOdarchuk/monokasa/internal/auth"
	"github.com/OlexiyOdarchuk/monokasa/internal/bot"
	"github.com/OlexiyOdarchuk/monokasa/internal/config"
	"github.com/OlexiyOdarchuk/monokasa/internal/email"
	"github.com/OlexiyOdarchuk/monokasa/internal/metrics"
	"github.com/OlexiyOdarchuk/monokasa/internal/og"
	"github.com/OlexiyOdarchuk/monokasa/internal/pay"
	"github.com/OlexiyOdarchuk/monokasa/internal/posters"
	"github.com/OlexiyOdarchuk/monokasa/internal/public"
	"github.com/OlexiyOdarchuk/monokasa/internal/realtime"
	"github.com/OlexiyOdarchuk/monokasa/internal/store"
	"github.com/OlexiyOdarchuk/monokasa/internal/ticket"
	"github.com/OlexiyOdarchuk/monokasa/internal/timefmt"
	"github.com/OlexiyOdarchuk/monokasa/internal/token"
	"github.com/OlexiyOdarchuk/monokasa/internal/web"
	"github.com/OlexiyOdarchuk/monokasa/internal/webui"
)

// fatal logs at error level and exits non-zero. slog has no built-in Fatal —
// this helper keeps the call sites short while preserving structured fields.
func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		fatal("config", "err", err)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		fatal("open store", "err", err)
	}
	defer st.Close()

	ctx, cancel := signalCtx()
	defer cancel()

	if _, err := st.SeedIfEmpty(ctx, store.Show{
		Title:    cfg.ShowTitle,
		Venue:    cfg.ShowVenue,
		StartsAt: cfg.ShowStartsAt,
	}, cfg.Rows, cfg.Cols, cfg.PriceKopecks); err != nil {
		fatal("seed", "err", err)
	}
	if sh, err := st.ActiveShow(ctx); err == nil {
		slog.Info("show ready",
			"id", sh.ID,
			"title", sh.Title,
			"startsAt", timefmt.DateTime(sh.StartsAt))
	}

	coder := token.NewCoder(cfg.Secret)

	// Realtime hub is a single instance shared by every producer
	// (public reserve, pay confirm, admin cancel, bot reserve/cancel,
	// sweep) and every SSE subscriber. In-process — single process,
	// no fan-out across replicas needed.
	hub := realtime.New()

	// Bot and pay both pull the active show on demand via ShowFn so admin
	// edits propagate without a restart, and a new show created after a
	// run-down period becomes the one bot/pay use automatically.
	botShowFn := func(ctx context.Context) (bot.Show, error) {
		sh, err := st.ActiveShow(ctx)
		if err != nil {
			return bot.Show{}, err
		}
		return bot.Show{ID: sh.ID, Title: sh.Title, Venue: sh.Venue, StartsAt: sh.StartsAt}, nil
	}
	payShowFn := func(ctx context.Context) (pay.Show, error) {
		sh, err := st.ActiveShow(ctx)
		if err != nil {
			return pay.Show{}, err
		}
		return pay.Show{Slug: sh.Slug, Title: sh.Title, Venue: sh.Venue, StartsAt: sh.StartsAt}, nil
	}

	tg, err := bot.New(bot.Options{
		Token:     cfg.TGToken,
		Store:     botStore{st},
		Coder:     coder,
		ShowFn:    botShowFn,
		BaseURL:   cfg.BaseURL,
		JarLink:   cfg.JarLink,
		Hold:      cfg.HoldDuration,
		AdminTGID: cfg.AdminTGID,
		Hub:       hub,
	})
	if err != nil {
		fatal("bot init", "err", err)
	}
	go tg.Start()
	defer tg.Stop()
	slog.Info("telegram bot up")

	// SMTP wiring is optional. Without SMTPHost+SMTPFrom we run without
	// email delivery; web-buyer reservations still confirm but the
	// processor logs a warn for each one until the operator wires it up.
	var emailDelivery pay.EmailDelivery
	if cfg.SMTPHost != "" && cfg.SMTPFrom != "" {
		sender, err := email.NewSMTPSender(email.Config{
			Host:        cfg.SMTPHost,
			Port:        cfg.SMTPPort,
			Username:    cfg.SMTPUser,
			Password:    cfg.SMTPPass,
			From:        cfg.SMTPFrom,
			ImplicitTLS: cfg.SMTPImplicitTLS,
		})
		if err != nil {
			fatal("smtp init", "err", err)
		}
		emailDelivery = payEmail{sender: sender, from: cfg.SMTPFrom}
		slog.Info("smtp ready", "host", cfg.SMTPHost, "from", cfg.SMTPFrom)
	} else {
		slog.Info("smtp not configured; email delivery disabled (web buyers won't receive PDFs)")
	}

	monoClient := bank.New()
	processor := &pay.Processor{
		Store:    payStore{st},
		Coder:    coder,
		Notifier: payNotifier{tg},
		Renderer: payRenderer,
		ShowFn:   payShowFn,
		Email:    emailDelivery,
		Hub:      hub,
	}
	hook, err := webhook.NewHandler(ctx, webhook.Options{
		Keys:    monoClient,
		Dedup:   webhook.NewMemoryDeduper(2048),
		OnEvent: processor.Handle,
		OnError: func(err error) {
			metrics.WebhookErrors.Add(1)
			slog.Error("webhook", "err", err)
		},
	})
	if err != nil {
		fatal("webhook handler init", "err", err)
	}
	slog.Info("mono webhook ready", "keyId", hook.KeyID())

	// /reconcile rescue net + /jar balance — both optional. Reconciler
	// needs MONO_TOKEN; jar lookup needs a parseable MONO_JAR_LINK.
	if cfg.MonoToken != "" {
		// KeyedLimiter dispatches by accountID — Mono caps both
		// /personal/client-info and each /personal/statement/{account}
		// at 1 call per 60s. ClientInfo uses the empty-key bucket;
		// per-account statements get their own buckets through
		// monobank.WithLimiterKey set in monoReconciler.walk.
		// idleTTL=0: keyspace is bounded (one bucket per account/jar from
		// ClientInfo), so no need for a background eviction sweeper.
		klim := monobank.NewKeyedLimiter(time.Minute, 1, 0)
		cli := personal.New(cfg.MonoToken, monobank.WithRateLimiter(klim))
		tg.SetReconciler(&monoReconciler{cli: cli, proc: processor})
	}
	if shortID := jarShortID(cfg.JarLink); shortID != "" {
		jcli, err := jar.New()
		if err != nil {
			fatal("jar client init", "err", err)
		}
		tg.SetJar(&jarLookup{cli: jcli, shortID: shortID})
	} else {
		slog.Info("jar command disabled (no short id in link)", "jarLink", cfg.JarLink)
	}

	if err := bootstrapAdmin(ctx, st, cfg.AdminEmail, cfg.AdminPassword); err != nil {
		fatal("bootstrap admin", "err", err)
	}

	authHandler := auth.NewHandler(st, cfg.SecureCookies)

	scanner := web.NewScanner(webStore{st}, coder, cfg.ScannerToken)

	// Seat gauges resolve the active show at scrape time and stat against
	// its id — so admins flipping between shows in the web UI don't need
	// to think about which one the metrics reflect (it's whatever's
	// currently active).
	expvar.Publish("monokasa_seats_sold", expvar.Func(func() any {
		return liveActiveStat(st, func(s store.Stats) int { return s.Sold })
	}))
	expvar.Publish("monokasa_seats_held", expvar.Func(func() any {
		return liveActiveStat(st, func(s store.Stats) int { return s.Held })
	}))
	expvar.Publish("monokasa_seats_free", expvar.Func(func() any {
		return liveActiveStat(st, func(s store.Stats) int { return s.Free })
	}))

	spa, err := webui.New()
	if err != nil {
		fatal("webui", "err", err)
	}

	// Admin API gets its own sub-mux so we can wrap the whole thing in
	// auth.RequireAuth once, rather than decorating every endpoint.
	adminMux := http.NewServeMux()
	adminH := admin.NewHandler(st)
	adminH.SetHub(hub)
	// Waitlist notifier — declared up here because the admin cancel
	// callback below closes over it. Set later (after we know we have
	// SMTP) but Go captures by reference, so the late assignment lands.
	var notifyWaitlist waitlistNotifyFn
	// Best-effort notification fan-out: when admin force-cancels a
	// reservation, ping the buyer through whichever channels are
	// attached to their row. Failures get logged inside; the cancel
	// itself is already durable in the DB.
	adminH.SetCancelNotifier(func(ctx context.Context, res store.Reservation, seat store.Seat) {
		if res.TGChatID != 0 {
			if err := tg.SendCancellation(res.TGChatID, toBotSeat(seat)); err != nil {
				slog.Warn("cancel notify: telegram", "chatId", res.TGChatID, "err", err)
			}
		}
		if res.BuyerEmail != "" && emailDelivery != nil {
			sh, err := payShowFn(ctx)
			if err != nil {
				slog.Warn("cancel notify: resolve show", "err", err)
				return
			}
			paySeat := pay.Seat{ID: seat.ID, Row: seat.Row, Col: seat.Col,
				Category: seat.Category,
				Price:    money.New(seat.PriceKopecks, currency.UAH)}
			if err := emailDelivery.SendCancellationEmail(ctx, res.BuyerEmail, res.BuyerName, paySeat, sh); err != nil {
				slog.Warn("cancel notify: email", "to", res.BuyerEmail, "err", err)
			}
		}
		// Waitlist: one freed seat = one next-in-line invite.
		if notifyWaitlist != nil {
			notifyWaitlist(ctx, seat.ShowID, 1)
		}
	})
	adminH.Register(adminMux)

	postersSvc, err := posters.New(cfg.PostersDir)
	if err != nil {
		fatal("posters", "err", err)
	}
	// Upload sits behind admin auth (writes to disk + serves URL back);
	// serve is public (it's the URL we ship to every landing/event card).
	adminMux.HandleFunc("POST /api/admin/posters", postersSvc.HandleUpload)

	// Magic-link login wires SMTP into the public side. Optional —
	// without SMTP_HOST set, payEmail is nil and /api/public/login/*
	// returns 503 login_disabled, but everything else keeps working.
	var loginMailer public.LoginMailer
	if emailDelivery != nil {
		loginMailer = buyerLoginMailer{sender: emailDelivery.(payEmail).sender, from: cfg.SMTPFrom}
	}

	publicHandler := public.NewHandler(public.Config{
		Store:         st,
		Coder:         coder,
		JarLink:       cfg.JarLink,
		Hold:          cfg.HoldDuration,
		MinPrice:      cfg.PriceKopecks,
		BotUsername:   cfg.BotUsername,
		Hub:           hub,
		BaseURL:       cfg.BaseURL,
		LoginMailer:   loginMailer,
		SecureCookies: cfg.SecureCookies,
	})

	mux := http.NewServeMux()
	mux.Handle("/webhook", hook)
	mux.Handle("/debug/vars", expvar.Handler())
	mux.Handle("/api/admin/", authHandler.RequireAuth(adminMux))
	mux.HandleFunc("/posters/", postersSvc.HandleServe)
	publicHandler.Register(mux)
	authHandler.Register(mux)
	scanner.Register(mux)
	// Social-preview wrapper: GET /event/<slug> is the canonical URL we
	// hand out (in QR posters, "copy link", buyer emails). Bots scraping
	// it need OG meta tags they can read without running JS. Falls back
	// to the plain SPA shell when slug doesn't resolve so the SvelteKit
	// client still renders a 404 view.
	mux.HandleFunc("GET /event/{slug}", eventOGHandler(st, spa, cfg.BaseURL))
	// SPA on "/" is the catch-all — http.ServeMux picks the longest pattern
	// match, so the specific routes above (and /health below) win for their
	// exact paths; everything else falls through to the Svelte build.
	mux.Handle("/", spa)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := st.Ping(ctx); err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: mux}
	go func() {
		slog.Info("http listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal("http", "err", err)
		}
	}()
	defer func() {
		shutCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = srv.Shutdown(shutCtx)
	}()

	// Опціональна авто-реєстрація вебхуку у monobank. Mono на /personal/webhook
	// GET-пінгне URL — тому даємо HTTP-серверу секунду на старт. Без MONO_TOKEN
	// або WEBHOOK_URL крок пропускається.
	if cfg.MonoToken != "" && cfg.WebhookURL != "" {
		go registerWebhook(ctx, cfg.MonoToken, cfg.WebhookURL)
	}

	// Reminders.
	go runReminderLoop(ctx, st, tg, cfg.RemindBefore)

	// Eagerly free seats whose HOLD has lapsed without payment, so /seats
	// reflects reality even when no user has triggered a status read.
	// Notifies pending waitlisters as a side effect so the buyer who left
	// their email finds out before the next person grabs it.
	if mailer, ok := emailDelivery.(payEmail); ok {
		pm := mailer
		notifyWaitlist = makeWaitlistNotifier(st, &pm, cfg.BaseURL)
	} else {
		notifyWaitlist = func(_ context.Context, _ int64, _ int) {}
	}
	go runHoldSweeper(ctx, st, hub, notifyWaitlist, 5*time.Minute)

	// GC idle rate-limit buckets so the map doesn't grow over the show's lifetime.
	go scanner.RunGC(ctx, 10*time.Minute, 30*time.Minute)

	// Drop expired session rows hourly. Cheap query (indexed) with no impact
	// on user-visible behaviour — FindSession already rejects expired tokens
	// at read time; the sweep is just housekeeping.
	go runSessionSweeper(ctx, st, time.Hour)

	<-ctx.Done()
	slog.Info("shutting down")
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

// registerWebhook calls POST /personal/webhook so monobank starts pushing
// statement events at the configured URL. Two things can be unready at
// startup: our own HTTP server (race with the listener), and the public
// HTTPS path (TLS, tunnel, DNS). Mono pings the URL with GET before
// accepting the subscription, so both must be reachable. We retry with
// exponential backoff for ~30s; if it still fails, log and exit — the
// operator can register manually via curl.
func registerWebhook(ctx context.Context, token, url string) {
	cli := personal.New(token)
	backoff := time.Second
	for attempt := 1; attempt <= 5; attempt++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := cli.SetWebHook(rctx, url)
		cancel()
		if err == nil {
			slog.Info("registered webhook", "url", url, "attempt", attempt)
			return
		}
		slog.Warn("register webhook failed", "attempt", attempt, "max", 5, "err", err)
		backoff *= 2
	}
	slog.Error("register webhook giving up — register manually", "url", url)
}

// liveActiveStat resolves the active show and reads one field out of its
// store.Stats with a 1s timeout. Used by the expvar gauges; on failure it
// returns -1 so a flat-lined gauge in Prometheus is a visible signal
// rather than a stale value.
func liveActiveStat(st *store.Store, pick func(store.Stats) int) int {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	sh, err := st.ActiveShow(ctx)
	if err != nil {
		return -1
	}
	s, err := st.Stats(ctx, sh.ID)
	if err != nil {
		return -1
	}
	return pick(s)
}

// runHoldSweeper periodically cancels expired-but-unpaid reservations so
// their seats become free without waiting for someone to call /seats.
// Each freed seat is broadcast through the realtime hub so any live
// SSE subscriber sees the map flip back to "free" without a refresh.
func runHoldSweeper(ctx context.Context, st *store.Store, hub *realtime.Hub, notifyWaitlist waitlistNotifyFn, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	sweep := func() {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		freed, err := st.SweepExpiredHolds(ctx)
		if err != nil {
			slog.Error("sweep holds", "err", err)
			return
		}
		if len(freed) == 0 {
			return
		}
		slog.Info("sweep holds: freed expired reservations", "count", len(freed))
		dispatchFreedSeats(ctx, freed, hub, notifyWaitlist)
	}
	sweep() // fire once at startup to catch carry-over from before restart
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			sweep()
		}
	}
}

// waitlistNotifyFn is called once per show that just had seats freed.
// freedCount caps how many of the next-in-line waitlisters to email
// (we don't want to wake 100 people for a single freed seat).
type waitlistNotifyFn func(ctx context.Context, showID int64, freedCount int)

// dispatchFreedSeats fans out a list of just-freed seats: SSE publish
// for live UI updates, and one waitlist-notify call per affected show.
// Called from every freeing path (sweep, admin cancel, bot cascade)
// so the buyer experience stays uniform.
func dispatchFreedSeats(ctx context.Context, freed []store.Seat, hub *realtime.Hub, notify waitlistNotifyFn) {
	if len(freed) == 0 {
		return
	}
	perShow := make(map[int64]int, 4)
	for _, seat := range freed {
		hub.Publish(seat.ShowID, realtime.Event{
			Type: "seat_status", SeatID: seat.ID, Status: realtime.SeatFree,
		})
		perShow[seat.ShowID]++
	}
	if notify == nil {
		return
	}
	for showID, count := range perShow {
		notify(ctx, showID, count)
	}
}

// makeWaitlistNotifier builds the notifyWaitlist callback. Pops the
// next batch of unnotified waitlisters for the show, sends each one
// the "звільнилось місце" email. mailer can be nil — without SMTP,
// the waitlist still records signups but no email goes out.
func makeWaitlistNotifier(st *store.Store, mailer *payEmail, baseURL string) waitlistNotifyFn {
	if mailer == nil {
		return func(_ context.Context, _ int64, _ int) {}
	}
	origin := strings.TrimRight(baseURL, "/")
	return func(ctx context.Context, showID int64, freedCount int) {
		// Cap at 5 even when more seats freed at once — beyond that the
		// race to grab a seat gets unfair to anyone slower than the rest.
		batch := min(freedCount, 5)
		entries, err := st.PopWaitlistForShow(ctx, showID, batch)
		if err != nil {
			slog.Error("waitlist pop", "showId", showID, "err", err)
			return
		}
		if len(entries) == 0 {
			return
		}
		show, err := st.LoadShow(ctx, showID)
		if err != nil {
			slog.Error("waitlist: load show", "showId", showID, "err", err)
			return
		}
		eventURL := origin + "/event/" + show.Slug
		for _, e := range entries {
			if err := mailer.SendWaitlistFreedEmail(ctx, e.Email,
				pay.Show{Slug: show.Slug, Title: show.Title, Venue: show.Venue, StartsAt: show.StartsAt},
				eventURL); err != nil {
				slog.Warn("waitlist email", "to", e.Email, "err", err)
			}
		}
		slog.Info("waitlist notified", "showId", showID, "count", len(entries))
	}
}

// bootstrapAdmin seeds the first admin user when the DB is empty and the
// ADMIN_EMAIL/ADMIN_PASSWORD ENV pair is set. After the first run the
// caller should remove these from .env — they're a one-shot rescue door.
func bootstrapAdmin(ctx context.Context, st *store.Store, email, password string) error {
	if email == "" || password == "" {
		return nil
	}
	n, err := st.CountUsers(ctx)
	if err != nil {
		return fmt.Errorf("count users: %w", err)
	}
	if n > 0 {
		return nil // someone is already in there — never overwrite
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	if _, err := st.CreateUser(ctx, email, "Admin", hash); err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	slog.Warn("bootstrapped admin user — remove ADMIN_PASSWORD from environment",
		"email", email)
	return nil
}

// runSessionSweeper periodically deletes expired session rows.
func runSessionSweeper(ctx context.Context, st *store.Store, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	sweep := func() {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		n, err := st.SweepExpiredSessions(ctx)
		if err != nil {
			slog.Error("sweep sessions", "err", err)
			return
		}
		if n > 0 {
			slog.Info("sweep sessions: deleted expired", "count", n)
		}
	}
	sweep()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			sweep()
		}
	}
}

// runReminderLoop wakes up periodically; when we're within
// `remindBefore` of the active show's start, it pings every
// paid-and-not-yet-reminded reservation once, then idles. The DB column
// reminded_at guarantees at-most-once delivery across restarts. The
// active-show is re-resolved each tick so admin edits to StartsAt take
// effect without a restart.
func runReminderLoop(ctx context.Context, st *store.Store, tg *bot.Bot, remindBefore time.Duration) {
	tick := time.NewTicker(1 * time.Minute)
	defer tick.Stop()
	check := func() {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		show, err := st.ActiveShow(ctx)
		if err != nil {
			return // no active show — nothing to remind about
		}
		until := time.Until(show.StartsAt)
		if until > remindBefore || until < -2*time.Hour {
			return // too early or already long over
		}
		items, err := st.ConfirmedNotYetReminded(ctx, show.ID)
		if err != nil {
			slog.Error("remind", "err", err)
			return
		}
		for _, it := range items {
			if err := tg.NotifyShowSoon(it.Reservation.TGChatID, toBotSeat(it.Seat), show.StartsAt); err != nil {
				slog.Warn("remind notify", "chatId", it.Reservation.TGChatID, "err", err)
				continue
			}
			if err := st.MarkReminded(ctx, it.Reservation.ID); err != nil {
				slog.Error("mark reminded", "reservationId", it.Reservation.ID, "err", err)
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

// botStore wraps *store.Store to satisfy bot.Store. The store layer speaks
// raw kopecks (matches the SQLite INTEGER column); the bot layer speaks
// money.Money so prices format themselves and carry their currency. This
// adapter does the conversion.
type botStore struct{ s *store.Store }

func toBotSeat(s store.Seat) bot.Seat {
	return bot.Seat{
		ID:       s.ID,
		ShowID:   s.ShowID,
		Row:      s.Row,
		Col:      s.Col,
		Price:    money.New(s.PriceKopecks, currency.UAH),
		Sellable: s.Sellable,
	}
}

func fromBotSeat(s bot.Seat) store.Seat {
	return store.Seat{
		ID: s.ID, ShowID: s.ShowID, Row: s.Row, Col: s.Col,
		PriceKopecks: s.Price.Minor, Sellable: s.Sellable,
	}
}

func toBotShow(s store.Show) bot.Show {
	return bot.Show{
		ID: s.ID, Slug: s.Slug, Title: s.Title, Venue: s.Venue,
		StartsAt: s.StartsAt, Description: s.Description,
	}
}

func (b botStore) Seats(ctx context.Context, showID int64) ([]bot.Seat, error) {
	seats, err := b.s.Seats(ctx, showID)
	if err != nil {
		return nil, err
	}
	out := make([]bot.Seat, len(seats))
	for i, s := range seats {
		out[i] = toBotSeat(s)
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
	return toBotSeat(s), translateStoreErr(err)
}

func (b botStore) Reserve(
	ctx context.Context, seat bot.Seat, tgUserID, tgChatID int64,
	buyerName, code string, hold time.Duration,
) (bot.Reservation, error) {
	// Bot users get tickets via Telegram → no email channel involved,
	// buyer_email column stays empty.
	r, err := b.s.Reserve(ctx, fromBotSeat(seat), tgUserID, tgChatID, buyerName, "", code, hold)
	return toBotReservation(r), translateStoreErr(err)
}

// CreateOrder groups N seats under one payment code for the bot's
// in-chat multi-seat picker. Bot buyers have no email column — the
// PDF batch is delivered via Telegram only.
func (b botStore) CreateOrder(
	ctx context.Context, seats []bot.Seat, tgUserID, tgChatID int64,
	buyerName, code string, hold time.Duration,
) (bot.Order, []bot.OrderItem, error) {
	storeSeats := make([]store.Seat, len(seats))
	for i, s := range seats {
		storeSeats[i] = fromBotSeat(s)
	}
	// Bot multi-seat doesn't collect per-attendee names today — every PDF
	// prints the buyer's name. Pass nil so all rows fall back at render.
	o, reservations, err := b.s.CreateOrder(ctx, storeSeats, tgUserID, tgChatID, buyerName, "", nil, code, hold)
	if err != nil {
		return bot.Order{}, nil, translateStoreErr(err)
	}
	botOrder := bot.Order{
		ID: o.ID, Code: o.Code,
		BuyerName: o.BuyerName, BuyerEmail: o.BuyerEmail,
		TGChatID:    o.TGChatID,
		ConfirmedAt: o.ConfirmedAt,
	}
	items := make([]bot.OrderItem, len(reservations))
	for i, r := range reservations {
		items[i] = bot.OrderItem{
			Reservation: toBotReservation(r),
			Seat:        toBotSeat(storeSeats[i]),
		}
	}
	return botOrder, items, nil
}

// Shows lists non-archived shows — same predicate the public landing
// uses, so the bot афіша doesn't drift from monokasa.app/. Past-dated
// events are deliberately shown (admin controls visibility through
// archive).
func (b botStore) Shows(ctx context.Context) ([]bot.Show, error) {
	shows, err := b.s.ListShows(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]bot.Show, 0, len(shows))
	for _, sh := range shows {
		if sh.ArchivedAt != nil {
			continue
		}
		out = append(out, toBotShow(sh))
	}
	return out, nil
}

func (b botStore) FindShowBySlug(ctx context.Context, slug string) (bot.Show, error) {
	sh, err := b.s.LoadShowBySlug(ctx, slug)
	if errors.Is(err, store.ErrShowNotFound) {
		return bot.Show{}, bot.ErrShowNotFound
	}
	if err != nil {
		return bot.Show{}, err
	}
	return toBotShow(sh), nil
}

func toBotReservation(r store.Reservation) bot.Reservation {
	return bot.Reservation{
		ID:          r.ID,
		SeatID:      r.SeatID,
		TGUserID:    r.TGUserID,
		TGChatID:    r.TGChatID,
		BuyerName:   r.BuyerName,
		Code:        r.Code,
		CreatedAt:   r.CreatedAt,
		ExpiresAt:   r.ExpiresAt,
		ConfirmedAt: r.ConfirmedAt,
	}
}

func (b botStore) CancelReservation(ctx context.Context, code string, tgUserID int64) (bot.Reservation, bot.Seat, error) {
	r, s, err := b.s.CancelReservation(ctx, code, tgUserID)
	return toBotReservation(r), toBotSeat(s), translateStoreErr(err)
}

func (b botStore) CancelHeldOrderByUser(ctx context.Context, orderCode string, tgUserID int64) ([]bot.Seat, error) {
	seats, err := b.s.CancelHeldOrderByUser(ctx, orderCode, tgUserID)
	if err != nil {
		return nil, translateStoreErr(err)
	}
	out := make([]bot.Seat, len(seats))
	for i, s := range seats {
		out[i] = toBotSeat(s)
	}
	return out, nil
}

func (b botStore) LinkOrderToTGChat(ctx context.Context, code string, tgUserID, tgChatID int64) (bot.Order, []bot.OrderItem, error) {
	o, items, err := b.s.LinkOrderToTGChat(ctx, code, tgUserID, tgChatID)
	if err != nil {
		return bot.Order{}, nil, translateStoreErr(err)
	}
	botOrder := bot.Order{
		ID: o.ID, Code: o.Code,
		BuyerName: o.BuyerName, BuyerEmail: o.BuyerEmail,
		TGChatID:    o.TGChatID,
		ConfirmedAt: o.ConfirmedAt,
	}
	botItems := make([]bot.OrderItem, len(items))
	for i, it := range items {
		botItems[i] = bot.OrderItem{
			Reservation: toBotReservation(it.Reservation),
			Seat:        toBotSeat(it.Seat),
		}
	}
	return botOrder, botItems, nil
}

func (b botStore) MyReservations(ctx context.Context, tgUserID int64) ([]bot.MyItem, error) {
	items, err := b.s.MyReservations(ctx, tgUserID)
	if err != nil {
		return nil, err
	}
	out := make([]bot.MyItem, len(items))
	for i, it := range items {
		out[i] = bot.MyItem{Reservation: toBotReservation(it.Reservation), Seat: toBotSeat(it.Seat)}
	}
	return out, nil
}

func (b botStore) Stats(ctx context.Context, showID int64) (bot.Stats, error) {
	s, err := b.s.Stats(ctx, showID)
	return bot.Stats{
		Total:   s.Total,
		Sold:    s.Sold,
		Held:    s.Held,
		Free:    s.Free,
		Revenue: money.New(s.RevenueKopecks, currency.UAH),
	}, err
}

func (b botStore) LogAudit(ctx context.Context, action, target, actorLabel, detailsJSON string) error {
	return b.s.LogAudit(ctx, store.AuditEntry{
		ActorUserID: 0,
		ActorEmail:  actorLabel,
		Action:      action,
		Target:      target,
		Details:     detailsJSON,
	})
}

func translateStoreErr(err error) error {
	switch err {
	case nil:
		return nil
	case store.ErrCodeNotFound:
		return bot.ErrCodeNotFound
	case store.ErrShowNotFound:
		return bot.ErrShowNotFound
	case store.ErrSeatTaken:
		return bot.ErrSeatTaken
	case store.ErrSeatNotFound, store.ErrSeatNotSellable:
		return bot.ErrSeatNotFound
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

func (p payStore) FindOrderByCode(ctx context.Context, code string) (pay.Order, []pay.OrderItem, error) {
	o, items, err := p.s.FindOrderByCode(ctx, code)
	payOrder := pay.Order{
		ID: o.ID, Code: o.Code,
		BuyerName:    o.BuyerName,
		BuyerEmail:   o.BuyerEmail,
		TGChatID:     o.TGChatID,
		TotalKopecks: o.TotalKopecks,
		ConfirmedAt:  o.ConfirmedAt,
	}
	switch {
	case errors.Is(err, store.ErrCodeNotFound):
		return payOrder, nil, pay.ErrCodeNotFound
	case errors.Is(err, store.ErrAlreadyClosed):
		return payOrder, nil, pay.ErrAlreadyClosed
	case err != nil:
		return payOrder, nil, err
	}
	payItems := make([]pay.OrderItem, len(items))
	for i, it := range items {
		payItems[i] = pay.OrderItem{
			ReservationID: it.Reservation.ID,
			AttendeeName:  it.Reservation.AttendeeName,
			Seat: pay.Seat{
				ID: it.Seat.ID, ShowID: it.Seat.ShowID,
				Row: it.Seat.Row, Col: it.Seat.Col,
				Category: it.Seat.Category,
				Price:    money.New(it.Seat.PriceKopecks, currency.UAH),
			},
		}
	}
	return payOrder, payItems, nil
}

func (p payStore) ConfirmOrder(ctx context.Context, orderID int64, qrPayloads map[int64]string) error {
	_, err := p.s.ConfirmOrder(ctx, orderID, qrPayloads)
	return err
}

// LogAudit lets pay.Processor write to the same audit_log table the
// admin/public flows use. ActorUserID stays 0 — payment.confirm is a
// system event triggered by monobank webhook, not by an admin.
func (p payStore) LogAudit(ctx context.Context, action, target, actorLabel, detailsJSON string) error {
	return p.s.LogAudit(ctx, store.AuditEntry{
		ActorUserID: 0,
		ActorEmail:  actorLabel,
		Action:      action,
		Target:      target,
		Details:     detailsJSON,
	})
}

type payNotifier struct{ b *bot.Bot }

func (p payNotifier) SendTicket(chatID int64, seat pay.Seat, pdf []byte) error {
	return p.b.SendTicket(chatID, bot.Seat{
		ID: seat.ID, Row: seat.Row, Col: seat.Col, Price: seat.Price,
	}, pdf)
}

// payEmail adapts the SMTPSender to pay.EmailDelivery, composing the
// subject/body here so the email package stays domain-agnostic.
type payEmail struct {
	sender *email.SMTPSender
	from   string
}

// buyerLoginMailer composes the magic-link email for /my login.
type buyerLoginMailer struct {
	sender *email.SMTPSender
	from   string
}

func (m buyerLoginMailer) SendLoginLink(ctx context.Context, to, link string) error {
	body := fmt.Sprintf(`<!doctype html>
<html><body style="font-family:system-ui,sans-serif;color:#111;line-height:1.5">
<h2 style="margin:0 0 .5em">Вхід на monokasa</h2>
<p>Тицьни на кнопку, щоб побачити всі свої квитки на цьому email:</p>
<p style="margin:1.5em 0">
  <a href="%s" style="background:#fbbe2c;color:#000;padding:.7em 1.2em;border-radius:.4em;text-decoration:none;font-weight:600">
    Відкрити мої квитки
  </a>
</p>
<p style="color:#666;font-size:.875em">Посилання живе 15 хвилин і працює лише один раз. Якщо ти не запитував — ігноруй цей лист.</p>
</body></html>`, htmlEscape(link))
	return m.sender.Send(ctx, email.Message{
		To:       to,
		Subject:  "Вхід на monokasa — твої квитки",
		HTMLBody: body,
	})
}

// SendWaitlistFreedEmail tells a waitlisted buyer that at least one
// seat freed up on the show they were watching. The link points at
// the public event page so they can race to grab a seat — the call
// site doesn't guarantee anything is still available by the time the
// email lands (someone else could grab it first).
func (p payEmail) SendWaitlistFreedEmail(ctx context.Context, to string, show pay.Show, eventURL string) error {
	body := fmt.Sprintf(`<!doctype html>
<html><body style="font-family:system-ui,sans-serif;color:#111;line-height:1.5">
<h2 style="margin:0 0 .5em">Звільнилось місце на «%s»!</h2>
<p>Хтось зняв бронь — у тебе є шанс встигнути.</p>
<p style="margin:1.5em 0">
  <a href="%s" style="background:#fbbe2c;color:#000;padding:.7em 1.2em;border-radius:.4em;text-decoration:none;font-weight:600">
    Подивитись вільні місця →
  </a>
</p>
<p><b>Коли:</b> %s<br>
<b>Де:</b> %s</p>
<p style="color:#666;font-size:.875em">Поспіши — якщо хтось встигне раніше, доведеться чекати знов. Це сповіщення прийде лише раз; повторно записатись можна тільки якщо квиток забере хтось інший.</p>
</body></html>`,
		htmlEscape(show.Title),
		htmlEscape(eventURL),
		timefmt.DateTime(show.StartsAt),
		htmlEscape(show.Venue))
	return p.sender.Send(ctx, email.Message{
		To:       to,
		Subject:  fmt.Sprintf("Звільнилось місце · «%s»", show.Title),
		HTMLBody: body,
	})
}

func (p payEmail) SendCancellationEmail(ctx context.Context, to, buyerName string, seat pay.Seat, show pay.Show) error {
	body := fmt.Sprintf(`<!doctype html>
<html><body style="font-family:system-ui,sans-serif;color:#111;line-height:1.5">
<h2 style="margin:0 0 .5em">Бронь на «%s» скасована</h2>
<p>Привіт, %s.<br>
Адміністратор скасував бронь на місце <b>ряд %d · %d</b> для події «%s» (%s).</p>
<p>Якщо оплата вже пройшла — повернення грошей буде вручну через monobank. Питання — дай відповідь на цей лист.</p>
</body></html>`,
		htmlEscape(show.Title), htmlEscape(buyerName),
		seat.Row, seat.Col,
		htmlEscape(show.Title), timefmt.DateTime(show.StartsAt))
	return p.sender.Send(ctx, email.Message{
		To:       to,
		Subject:  fmt.Sprintf("Бронь скасована · «%s»", show.Title),
		HTMLBody: body,
	})
}

func (p payEmail) SendTicketBatchEmail(ctx context.Context, to, buyerName string, items []pay.EmailItem, show pay.Show) error {
	if len(items) == 0 {
		return nil
	}
	var seatList strings.Builder
	attachments := make([]email.Attachment, 0, len(items)+1)
	for _, it := range items {
		fmt.Fprintf(&seatList, "<li>ряд %d · місце %d</li>", it.Seat.Row, it.Seat.Col)
		attachments = append(attachments, email.Attachment{
			Filename:    fmt.Sprintf("ticket-row-%d-seat-%d.pdf", it.Seat.Row, it.Seat.Col),
			Body:        it.PDF,
			ContentType: "application/pdf",
		})
	}
	// One .ics per email, not per ticket — multi-seat order is one
	// event in the buyer's calendar. Apple Mail and Gmail render an
	// "Add to calendar" button when they see text/calendar.
	attachments = append(attachments, email.Attachment{
		Filename: "event.ics",
		Body: email.BuildICS(email.EventInvite{
			Title:     show.Title,
			Venue:     show.Venue,
			StartsAt:  show.StartsAt,
			StableID:  show.Slug,
			Organizer: p.from,
		}),
		ContentType: "text/calendar; method=PUBLISH; charset=utf-8",
	})
	noun := "квиток"
	if len(items) > 1 {
		noun = fmt.Sprintf("квитки (%d шт.)", len(items))
	}
	body := fmt.Sprintf(`<!doctype html>
<html><body style="font-family:system-ui,sans-serif;color:#111;line-height:1.5">
<h2 style="margin:0 0 .5em">Твої %s на «%s»</h2>
<p>Привіт, %s!<br>
Оплата зарахована — %s у вкладеннях.</p>
<p><b>Місця:</b></p>
<ul>%s</ul>
<p><b>Коли:</b> %s<br>
<b>Де:</b> %s</p>
<p>На вході покажи QR з PDF — ми відскануємо.</p>
<p style="color:#666;font-size:.875em">Питання? Просто дайте відповідь на цей лист.</p>
</body></html>`,
		htmlEscape(noun), htmlEscape(show.Title),
		htmlEscape(buyerName), htmlEscape(noun),
		seatList.String(),
		timefmt.DateTime(show.StartsAt), htmlEscape(show.Venue))

	subject := fmt.Sprintf("Твій квиток на «%s»", show.Title)
	if len(items) > 1 {
		subject = fmt.Sprintf("Твої квитки (%d) на «%s»", len(items), show.Title)
	}
	return p.sender.Send(ctx, email.Message{
		To:          to,
		Subject:     subject,
		HTMLBody:    body,
		Attachments: attachments,
	})
}

// htmlEscape is a tiny replacement for html.EscapeString to avoid pulling
// in the whole html package just to escape four characters.
func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
}

func payRenderer(show pay.Show, seat pay.Seat, buyerName, qrPayload string) ([]byte, error) {
	return ticket.RenderPDF(
		ticket.Show{Title: show.Title, Venue: show.Venue, StartsAt: show.StartsAt},
		ticket.Seat{Row: seat.Row, Col: seat.Col, Category: seat.Category},
		buyerName, qrPayload,
	)
}

// eventOGHandler serves the SPA shell with route-specific Open Graph
// tags injected so social bots (Telegram, FB, Twitter, Slack, Discord)
// get a rich preview when someone pastes the URL. Falls back to the
// plain SPA if the slug doesn't resolve — the client-side router will
// render its own 404 view in that case.
func eventOGHandler(st *store.Store, spa *webui.Handler, baseURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")
		if slug == "" {
			spa.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		show, err := st.LoadShowBySlug(ctx, slug)
		if err != nil {
			// Unknown slug or archived → let the SPA render its own
			// "not found" view. Errors from the DB shouldn't break the
			// page either; if SQLite is busy we still want to serve the
			// shell, the client will then surface a friendlier message.
			spa.ServeHTTP(w, r)
			return
		}
		origin := strings.TrimRight(baseURL, "/")
		if origin == "" {
			scheme := "https"
			if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
				scheme = "http"
			}
			origin = scheme + "://" + r.Host
		}
		props := og.Props{
			URL:         origin + "/event/" + show.Slug,
			Title:       show.Title,
			Description: show.Description,
			ImageURL:    og.AbsoluteImageURL(origin, show.PosterURL),
			SiteName:    "monokasa",
			StartsAt:    show.StartsAt,
			Venue:       show.Venue,
		}
		body := og.Render(spa.IndexHTML(), props)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// Short cache so a description/poster edit flows out within a
		// reasonable window. Bots typically re-scrape on each share
		// anyway, but humans land on the same URL too.
		w.Header().Set("Cache-Control", "public, max-age=60")
		_, _ = w.Write(body)
	}
}

// --- /reconcile adapter ---

// monoReconciler walks every account + jar in ClientInfo over the given
// window and replays each transaction through pay.Processor. Mono
// rate-limits /personal/statement to 1 call per 60s per account, so a
// reconcile across multiple accounts can take a minute or two.
type monoReconciler struct {
	cli  *personal.Client
	proc *pay.Processor
}

func (r *monoReconciler) Reconcile(ctx context.Context, lookback time.Duration, progress func(string)) (bot.ReconcileResult, error) {
	if progress == nil {
		progress = func(string) {}
	}
	info, err := r.cli.ClientInfo(ctx)
	if err != nil {
		return bot.ReconcileResult{}, fmt.Errorf("client info: %w", err)
	}
	to := time.Now()
	from := to.Add(-lookback)
	totalSources := len(info.Accounts) + len(info.Jars)
	var res bot.ReconcileResult
	idx := 0
	walk := func(accountID, label string) error {
		idx++
		progress(fmt.Sprintf("джерело %d/%d: %s — скан транзакцій…", idx, totalSources, label))
		var seen int
		// Tag the per-account quota bucket on the context so the
		// KeyedLimiter wired into r.cli throttles each /personal/
		// statement/{account} call independently.
		walkCtx := monobank.WithLimiterKey(ctx, accountID)
		for tx, err := range r.cli.TransactionsRangeIter(walkCtx, accountID, from, to) {
			if err != nil {
				return fmt.Errorf("%s: %w", label, err)
			}
			seen++
			res.Scanned++
			matched, err := r.proc.ReconcileTx(ctx, tx)
			if err != nil {
				slog.Warn("reconcile tx", "source", label, "txId", tx.ID, "err", err)
				continue
			}
			if matched {
				res.Matched++
			}
		}
		progress(fmt.Sprintf("джерело %d/%d: %s — %d транзакцій, %d збігів",
			idx, totalSources, label, seen, res.Matched))
		return nil
	}
	for _, a := range info.Accounts {
		if err := walk(a.AccountID, "акаунт "+a.AccountID); err != nil {
			return res, err
		}
	}
	for _, j := range info.Jars {
		if err := walk(j.ID, "банка "+j.ID); err != nil {
			return res, err
		}
	}
	return res, nil
}

// --- /jar adapter ---

// jarLookup caches the long jar id (resolved once via send.monobank.ua)
// and queries /bank/jar for the balance.
type jarLookup struct {
	cli     *jar.Client
	shortID string

	mu     sync.Mutex
	longID string
}

func (j *jarLookup) Balance(ctx context.Context) (bot.JarBalance, error) {
	longID, err := j.resolveLongID(ctx)
	if err != nil {
		return bot.JarBalance{}, err
	}
	info, err := j.cli.ByLongID(ctx, longID)
	if err != nil {
		return bot.JarBalance{}, err
	}
	code := currency.Code(info.Currency)
	return bot.JarBalance{
		Title:   info.Title,
		Owner:   info.OwnerName,
		Balance: money.New(info.Amount, code),
		Goal:    money.New(info.Goal, code),
	}, nil
}

func (j *jarLookup) resolveLongID(ctx context.Context) (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.longID != "" {
		return j.longID, nil
	}
	send, err := j.cli.ByShortID(ctx, j.shortID)
	if err != nil {
		return "", fmt.Errorf("resolve short id %q: %w", j.shortID, err)
	}
	if send.LongJarID == "" {
		return "", fmt.Errorf("monobank did not return long jar id for %q", j.shortID)
	}
	j.longID = send.LongJarID
	return j.longID, nil
}

// jarShortID extracts the short jar id from a send.monobank.ua URL —
// the trailing path segment of "https://send.monobank.ua/<id>" or
// "https://send.monobank.ua/jar/<id>". Returns "" when the URL doesn't
// match either shape.
func jarShortID(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if p := parts[i]; p != "" && p != "jar" {
			return p
		}
	}
	return ""
}
