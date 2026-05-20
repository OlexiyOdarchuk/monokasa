# 50 · Package: bot

Telegram side of the app. Long-polling клієнт через `telebot.v3`.

## Public API

- `New(opts Options) (*Bot, error)` — створює бот з токеном і
  залежностями.
- `Start()` — блокуючий polling loop.
- `Stop()` — graceful shutdown.
- `SendTicket(chatID, seat, pdf)` — викликається з `pay.Processor`.
- `SendCancellation(chatID, seat)` — admin force-cancel callback.
- `NotifyShowSoon(chatID, seat, when)` — reminder loop.
- `SetReconciler(r)`, `SetJar(j)` — wired after construction (optional).

## Залежності

```go
type Store interface {
    Shows, FindShowBySlug, Seats, SeatStatuses, FindFreeSeat,
    Reserve, CreateOrder,                // single + multi
    CancelReservation, MyReservations,
    Stats, LinkOrderToTGChat
}
type Coder       interface { NewCode() (string, error) }
type Reconciler  interface { Reconcile(ctx, lookback, progress) (Result, error) }  // optional
type JarLookup   interface { Balance(ctx) (JarBalance, error) }                    // optional
type ShowFn      func(ctx) (Show, error)  // активна подія для /stats, reminders
```

## UX-модель

- `/start` без deep-link → афіша (inline keyboard з усіма
  non-archived shows).
- Tap event → меню події з кнопками:
  - `📋 Обрати місце` — in-chat seat picker з multi-seat кошиком
  - `🗺 Відкрити мапу залу` — Telegram WebApp (`/event/<slug>`), якщо
    заданий `BASE_URL`
  - `↩ До списку`
- `/start res_<code>` — deep link з web-flow, прив'язує чат до order.

Детальний flow → [[40-flows/buyer-bot]].

## In-chat multi-seat picker

Стан тримається у `sync.Map` (key = tg user id, value = `pendingPick`).
Re-render у відповідь на tap через `c.Edit(text, markup)`.

```go
type pendingPick struct {
    ShowID int64
    Slug   string
    Seats  []pickedSeat   // toggle-list, до 20
    AwaitingName bool     // після Завершити
    Until  time.Time      // TTL 10 хв
}
```

`sweepPending` чистить покинуті picks раз на хвилину.

## Callbacks

- `show|<slug>` → меню події
- `pick|<slug>` → seat board
- `seat|<slug>:<row>:<col>` → toggle
- `done|<slug>` → попросити ім'я (ForceReply)
- `clear|<slug>` → очистити кошик
- `cancel|<code>` → cancel single-seat (multi — через /my)
- `events|back` → афіша

## Кому показуються admin-команди

`b.adminTGID` — TG user id адміна (`ADMIN_TG_ID` ENV). Для не-адміна
`/stats`, `/reconcile`, `/jar` відповідають "⛔️ команда тільки для
адміна".

## Файли

- `bot.go` — все: types, Store interface, handlers, render helpers,
  formatters, ~1100 рядків
- `bot_test.go` — handler tests з fake Store

## Дотичне

- [[40-flows/buyer-bot]] — повний UX flow
- [[50-packages/store]] — джерело Shows/Seats/CreateOrder
