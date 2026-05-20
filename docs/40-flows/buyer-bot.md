# 40 · Flow: покупець через бот

## Сценарій

Користувач у Telegram натискає `/start` → бачить афішу → тапає подію →
бачить меню події → `📋 Обрати місце` → накопичує seats у кошик → `✅ Завершити` → вводить ім'я → отримує `💳 Оплатити` button → платить → отримує PDF у чат.

Альтернативно: у меню події є `🗺 Відкрити мапу залу` — Telegram WebApp,
що відкриває `/event/<slug>` як Mini App усередині Telegram. Той же
flow, що [[buyer-web]].

## Послідовність

```
buyer (TG)         bot                store              monobank
   │                │                    │                   │
   │ /start         │                    │                    │
   │───────────────►│ handleStart → sendEventList            │
   │                │ store.Shows() ────►│                   │
   │                │◄───────────────────│ shows[]           │
   │◄───────────────│ inline kbd афіша                       │
   │                │                                         │
   │ tap show       │                                         │
   │───────────────►│ callbackShow                            │
   │                │ FindShowBySlug ───►│                    │
   │                │◄───────────────────│ Show              │
   │◄───────────────│ menu (📋 / 🗺 / ↩)                    │
   │                │                                         │
   │ tap 📋         │                                         │
   │───────────────►│ callbackPick                            │
   │                │ Seats + SeatStatuses ──────►│           │
   │                │ pending = {ShowID, Slug, Seats=[]}      │
   │◄───────────────│ inline grid (✓/✖/…/number)             │
   │                │                                         │
   │ tap seat       │                                         │
   │───────────────►│ callbackSeat → toggle pending.Seats     │
   │                │ FindFreeSeat (race-check)               │
   │◄───────────────│ c.Edit → re-render з ✓                 │
   │                │                                         │
   │ (repeat for more seats)                                  │
   │                │                                         │
   │ tap "✅ Завершити (N · sum)"                              │
   │───────────────►│ callbackDone                            │
   │                │ pending.AwaitingName = true             │
   │◄───────────────│ "Введи ім'я" + ForceReply               │
   │                │                                         │
   │ type name      │                                         │
   │───────────────►│ handleText (AwaitingName=true)          │
   │                │ FindFreeSeat × N                        │
   │                │ NewCode + CreateOrder([N seats]) ──►│   │
   │                │◄────────────────────────────────────│   │
   │◄───────────────│ "Забронював N місць"                   │
   │                │ + 💳 Оплатити button                   │
   │                │                                         │
   │ tap 💳 → opens monobank                                  │
   │─────────────────────────────────────────────────────────►│
   │                │                                         │
   │                │            POST /webhook ◄──────────────│
   │                │ pay.Processor:                          │
   │                │   FindOrderByCode (bare 8-char)         │
   │                │   ConfirmOrder + tickets                │
   │                │   render N PDF                          │
   │                │   bot.SendTicket × N (CTGChatID)        │
   │                │   (опційно) SMTP batch на BuyerEmail    │
   │                │                                         │
   │◄───────────────│ N документів × PDF                     │
```

## Стан bot.pendingPick

```go
pendingPick{
    ShowID, Slug,
    Seats: []pickedSeat{ {SeatID, Row, Col, Price}, ... },
    AwaitingName: bool,
    Until: time + 10m,
}
```

- callbackPick: ставить порожній basket → користувач починає вибір з нуля
- callbackSeat: toggle (add якщо нема, drop якщо є)
- callbackClear: reset basket, re-render
- callbackDone: вимагає ≥1; ставить AwaitingName=true
- handleText: тільки якщо AwaitingName → CreateOrder, drop pending
- sweepPending (1× на хв): дропає basket'и, де Until минув

## Deep link

`/start res_<code>` — викликає `linkReservation`:
- `store.LinkOrderToTGChat(code, tgUserID, tgChatID)` → апдейтить
  `orders.tg_user_id/tg_chat_id` і всі дочірні `reservations`.
- Якщо `ConfirmedAt != nil` → "вже оплачено, перевір email".
- Інакше → "підключив, PDF прийде сюди після оплати".

## Multi-seat: чим бот відрізняється від веб

- Per-seat attendee names — **не питає**. Усі PDF з ім'ям замовника.
  Так свідомо: на телефоні вводити N імен — неприйнятно.
- Кошик у пам'яті процесу (sync.Map) — після рестарту бота
  pending-стани втрачаються (TTL 10 хв все одно).
- Cancel button **тільки для single-seat** — для multi треба йти в
  `/my`.

## Edge cases

| Що | Як обробляється |
|---|---|
| Користувач почав picker, пішов пити каву > 10 хв | sweep дропає pending; наступний tap каже "натисни 📋 ще раз" |
| Натиснув "Завершити" з порожнім кошиком | "Спочатку обери хоч одне місце" як toast |
| Тапнув на чуже місце що щойно зайняли | "Це місце вже зайняте" toast, basket не міняється |
| 21-ше місце | "Максимум 20 місць" toast, не додається |
| Замість імені натиснув щось дивне | normalizeName помилка, "спробуй ще раз" — pending живе |
| Між Завершити і name input — хтось забрав місце | CreateOrder повертає ErrSeatTaken, friendly помилка, pending очищається |

## Файли

- `internal/bot/bot.go` — увесь bot UX
- `cmd/app/main.go` — `botStore` адаптер навколо `*store.Store`

## Дотичне

- [[50-packages/bot]] — як побудовано пакет
- [[40-flows/buyer-web]] — паралельний flow для веб-покупця
- [[40-flows/webhook]] — що відбувається після оплати
