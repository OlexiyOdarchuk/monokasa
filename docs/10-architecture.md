# 10 · Архітектура

## Однією картинкою

```
                       ┌─────────────────────────────┐
                       │       cmd/app/main.go       │
                       │   wiring + adapters + ENV   │
                       └──────────────┬──────────────┘
                                      │
       ┌──────────────────────────────┼──────────────────────────────┐
       │                              │                              │
       │ HTTP :8093                   │ Telegram long-poll            │ background
       ▼                              ▼                              ▼
┌──────────────┐               ┌──────────────┐              ┌───────────────┐
│   webhook    │               │     bot      │              │ reminder loop │
│ /webhook     │               │  Telegram    │              │  SweepExpired │
│              │               │              │              │               │
│ POST mono →  │               │ /events,/my, │              │ ConfirmedNot  │
│ pay.Process  │               │ pick, link…  │              │ YetReminded   │
└──────┬───────┘               └──────┬───────┘              └───────┬───────┘
       │                              │                              │
       └──────────────┬───────────────┴──────────────┬───────────────┘
                      │                              │
                      ▼                              ▼
              ┌──────────────────────────────────────────────────┐
              │            internal/store (SQLite, WAL)          │
              │ shows · seats · orders · reservations · tickets  │
              │      users · sessions                            │
              └──────────────────────────────────────────────────┘
                      ▲
                      │
                      │  ┌─ public  /api/public/*  (no auth)
                      ├──┤
                      │  │
                      │  └─ admin   /api/admin/*   (RequireAuth → auth)
                      │
                      └─ scan     /scan + /scan/check  (shared-token)
```

## Пакети і їхні ролі

| Пакет | Кому потрібен | За що відповідає |
|---|---|---|
| `cmd/app` | сам бінарник | wiring config → store → bot+pay+http; адаптери між пакетами; reminder loop |
| `internal/store` | всі | SQLite, схема, міграції, CRUD; домен сутностей (Show/Seat/Order/Reservation/Ticket/User/Session) |
| `internal/pay` | webhook, reconcile | match платежу за кодом → ConfirmOrder → render → доставка (TG/email) |
| `internal/bot` | telegram | /start, афіша, multi-seat picker, /my, /stats, /reconcile, /jar, deep-link |
| `internal/public` | веб-фронт | публічний API: список подій, деталі події, POST /orders, статус |
| `internal/admin` | веб-адмін | CRUD shows/seats, guests, force-cancel, ме |
| `internal/auth` | admin | bcrypt-сесії, login/logout, RequireAuth middleware |
| `internal/email` | pay | SMTP, multipart MIME, batch N attachments |
| `internal/posters` | admin | upload multipart, валідація MIME, static serve |
| `internal/ticket` | pay | рендер PDF A6 з QR |
| `internal/token` | pay, public, bot | 8-символьний код базою 32, HMAC payload для QR |
| `internal/web` | scanner | /scan UI + /scan/check, rate-limit, anti-replay |
| `internal/webui` | http | embed.FS handler для зібраного Svelte SPA |
| `internal/metrics` | http | expvar лічильники, читаються Prometheus textfile-exporter |
| `internal/timefmt` | bot | "5 червня 2026, 19:00" — український форматер |
| `internal/config` | main | ENV → Config |
| `internal/e2e` | тести | cross-package integration |

Деталі по кожному → каталог `50-packages/`.

## Dependency direction

**Внутрішні пакети НЕ імпортують один одного.** Кожен оголошує:
- власні типи (`bot.Show`, `pay.Order`, ...);
- інтерфейси для залежностей (`bot.Store`, `pay.Email`, ...);
- доменні помилки (`bot.ErrSeatTaken`, `pay.ErrCodeNotFound`).

`cmd/app/main.go` створює конкретні реалізації (звичайно `*store.Store`)
і обгортає їх у адаптери, що задовольняють чужі інтерфейси. Адаптер
переносить тип/помилку через кордон пакета.

Наслідок: будь-який пакет можна тестувати з фейк-Store без імпорту
SQLite. `pay.Processor` нічого не знає про `*store.Store` — тільки
про `pay.Store` interface.

## Чому моноліт, а не мікросервіси

- Один organizer = одна машина. Розділяти нема сенсу.
- Telegram-бот, webhook, web — все ділиться однією БД. Через мережу
  ділити SQLite — погана ідея.
- distroless image + Go-бінарник = ~30 МБ, нічого не варто додатково
  компонувати.

## Lifecycle бінарника (main.go)

1. `config.Load()` → ENV у структуру + валідація.
2. `store.Open()` → відкриває SQLite, прокручує міграції, опційно
   seed-ить перший show.
3. Створюються адаптери: `botStore`, `payStore`, `payEmail`, `botCoder`.
4. `bot.New()`, `pay.New()`, `public.NewHandler()`, `admin.NewHandler()`,
   `auth.NewHandler()`, `web.NewScanner()`, `posters.New()`,
   `webui.NewHandler()`.
5. `mux := http.NewServeMux()` → реєстрація: webhook, /api/admin/* (за
   RequireAuth middleware), /api/public/*, /admin/login, /admin/logout,
   /scan, /posters/, /health, /debug/vars, /.
6. `go runReminderLoop()` (повторно: 1 хв) — пінгає
   `ConfirmedNotYetReminded`, шле через bot, маркує `reminded_at`.
7. `go runSweepLoop()` — `SweepExpiredHolds` раз на хвилину.
8. `bot.Start()` (блокуючий), `http.ListenAndServe()` (в goroutine).
9. SIGTERM → graceful shutdown (bot.Stop, http.Shutdown, store.Close).

## Дотичне

- [[20-data-model]] — що лежить у `store`
- [[30-endpoints]] — повний реєстр маршрутів
- [[40-flows/buyer-web]] — як це працює end-to-end
