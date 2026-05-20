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
	"github.com/OlexiyOdarchuk/monokasa/internal/metrics"
	"github.com/OlexiyOdarchuk/monokasa/internal/pay"
	"github.com/OlexiyOdarchuk/monokasa/internal/public"
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

	showID, err := st.SeedIfEmpty(ctx, store.Show{
		Title:    cfg.ShowTitle,
		Venue:    cfg.ShowVenue,
		StartsAt: cfg.ShowStartsAt,
	}, cfg.Rows, cfg.Cols, cfg.PriceKopecks)
	if err != nil {
		fatal("seed", "err", err)
	}
	show, err := st.LoadShow(ctx, showID)
	if err != nil {
		fatal("load show", "err", err)
	}
	slog.Info("show ready",
		"id", show.ID,
		"title", show.Title,
		"startsAt", timefmt.DateTime(show.StartsAt))

	coder := token.NewCoder(cfg.Secret)

	tg, err := bot.New(bot.Options{
		Token:     cfg.TGToken,
		Store:     botStore{st},
		Coder:     coder,
		Show: bot.Show{
			ID:       show.ID,
			Title:    show.Title,
			Venue:    show.Venue,
			StartsAt: show.StartsAt,
		},
		JarLink:   cfg.JarLink,
		Hold:      cfg.HoldDuration,
		AdminTGID: cfg.AdminTGID,
	})
	if err != nil {
		fatal("bot init", "err", err)
	}
	go tg.Start()
	defer tg.Stop()
	slog.Info("telegram bot up")

	monoClient := bank.New()
	processor := &pay.Processor{
		Store:    payStore{st},
		Coder:    coder,
		Notifier: payNotifier{tg},
		Renderer: payRenderer,
		Show:     pay.Show{Title: show.Title, Venue: show.Venue, StartsAt: show.StartsAt},
		MinPrice: money.New(cfg.PriceKopecks, currency.UAH),
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

	// Seat gauges read straight from the store at scrape time — Prometheus
	// scrapes infrequently enough that this is cheaper than maintaining a
	// cached value in memory and keeping it consistent.
	expvar.Publish("monokasa_seats_sold", expvar.Func(func() any {
		return liveStat(st, show.ID, func(s store.Stats) int { return s.Sold })
	}))
	expvar.Publish("monokasa_seats_held", expvar.Func(func() any {
		return liveStat(st, show.ID, func(s store.Stats) int { return s.Held })
	}))
	expvar.Publish("monokasa_seats_free", expvar.Func(func() any {
		return liveStat(st, show.ID, func(s store.Stats) int { return s.Free })
	}))

	spa, err := webui.New()
	if err != nil {
		fatal("webui", "err", err)
	}

	// Admin API gets its own sub-mux so we can wrap the whole thing in
	// auth.RequireAuth once, rather than decorating every endpoint.
	adminMux := http.NewServeMux()
	admin.NewHandler(st).Register(adminMux)

	publicHandler := public.NewHandler(public.Config{
		Store:    st,
		Coder:    coder,
		JarLink:  cfg.JarLink,
		Hold:     cfg.HoldDuration,
		MinPrice: cfg.PriceKopecks,
	})

	mux := http.NewServeMux()
	mux.Handle("/webhook", hook)
	mux.Handle("/debug/vars", expvar.Handler())
	mux.Handle("/api/admin/", authHandler.RequireAuth(adminMux))
	publicHandler.Register(mux)
	authHandler.Register(mux)
	scanner.Register(mux)
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
	go runReminderLoop(ctx, st, tg, show, cfg.RemindBefore)

	// Eagerly free seats whose HOLD has lapsed without payment, so /seats
	// reflects reality even when no user has triggered a status read.
	go runHoldSweeper(ctx, st, 5*time.Minute)

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

// liveStat reads one field out of store.Stats with a 1s timeout. Used by
// the expvar gauges; on failure it returns -1 so a flat-lined gauge in
// Prometheus is a visible signal rather than a stale value.
func liveStat(st *store.Store, showID int64, pick func(store.Stats) int) int {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s, err := st.Stats(ctx, showID)
	if err != nil {
		return -1
	}
	return pick(s)
}

// runHoldSweeper periodically cancels expired-but-unpaid reservations so
// their seats become free without waiting for someone to call /seats.
func runHoldSweeper(ctx context.Context, st *store.Store, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	sweep := func() {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		n, err := st.SweepExpiredHolds(ctx)
		if err != nil {
			slog.Error("sweep holds", "err", err)
			return
		}
		if n > 0 {
			slog.Info("sweep holds: freed expired reservations", "count", n)
		}
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
		ID:     s.ID,
		ShowID: s.ShowID,
		Row:    s.Row,
		Col:    s.Col,
		Price:  money.New(s.PriceKopecks, currency.UAH),
	}
}

func fromBotSeat(s bot.Seat) store.Seat {
	return store.Seat{
		ID:           s.ID,
		ShowID:       s.ShowID,
		Row:          s.Row,
		Col:          s.Col,
		PriceKopecks: s.Price.Minor,
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

func (b botStore) Reserve(
	ctx context.Context, seat bot.Seat, tgUserID, tgChatID int64,
	buyerName, code string, hold time.Duration,
) (bot.Reservation, error) {
	// Bot users get their tickets through Telegram; no email channel
	// involved, so the buyer_email column stays empty for these rows.
	r, err := b.s.Reserve(ctx, fromBotSeat(seat), tgUserID, tgChatID, buyerName, "", code, hold)
	return toBotReservation(r), translateStoreErr(err)
}

func (b botStore) CancelReservation(ctx context.Context, code string, tgUserID int64) (bot.Reservation, bot.Seat, error) {
	r, s, err := b.s.CancelReservation(ctx, code, tgUserID)
	return toBotReservation(r), toBotSeat(s), translateStoreErr(err)
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
	seat := pay.Seat{ID: s.ID, Row: s.Row, Col: s.Col, Price: money.New(s.PriceKopecks, currency.UAH)}
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
		ID: seat.ID, Row: seat.Row, Col: seat.Col, Price: seat.Price,
	}, pdf)
}

func payRenderer(show pay.Show, seat pay.Seat, buyerName, qrPayload string) ([]byte, error) {
	return ticket.RenderPDF(
		ticket.Show{Title: show.Title, Venue: show.Venue, StartsAt: show.StartsAt},
		ticket.Seat{Row: seat.Row, Col: seat.Col},
		buyerName, qrPayload,
	)
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
