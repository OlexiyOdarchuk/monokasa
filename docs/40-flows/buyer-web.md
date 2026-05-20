# 40 · Flow: покупець через веб

## Сценарій

Користувач заходить з посилання на `https://your.host/event/<slug>`,
вибирає 1–20 місць, вводить ім'я і email, платить у моно, отримує PDF
на пошту (і опційно — у Telegram).

## Послідовність

```
buyer                      frontend                  backend                     monobank
  │                           │                         │                            │
  │ open /event/<slug>        │                         │                            │
  │──────────────────────────►│ GET /api/public/        │                            │
  │                           │     shows/<slug>        │                            │
  │                           │────────────────────────►│                            │
  │                           │◄────────────────────────│ seat map JSON              │
  │ tap seats                 │                         │                            │
  │ (toggle into basket)      │                         │                            │
  │                           │                         │                            │
  │ type name + email,        │                         │                            │
  │ optionally per-seat names │                         │                            │
  │                           │                         │                            │
  │ click "Забронювати"       │                         │                            │
  │──────────────────────────►│ POST /api/public/orders │                            │
  │                           │────────────────────────►│ CreateOrder (atomic)       │
  │                           │◄────────────────────────│ {code, pay_url, items, …}  │
  │ see success screen        │                         │                            │
  │                           │ poll status every 5s ──►│ GET /reservations/{code}/  │
  │                           │                         │     status → "held"        │
  │ click "Сплатити" →        │                         │                            │
  │──────────────────────────────────────────────────────────────────────────────────►│
  │                                                                                  │
  │ платить у моно                                                                   │
  │                                                                                  │
  │                                                     monobank POST /webhook ◄─────│
  │                                                     pay.Processor:               │
  │                                                       FindOrderByCode            │
  │                                                       ConfirmOrder               │
  │                                                       render N PDF               │
  │                                                       send email + (opt) TG      │
  │                                                                                  │
  │                           │ poll →                  │ ConfirmedAt set            │
  │                           │◄────────────────────────│ status "paid"              │
  │ "Оплачено! PDF на email"  │                         │                            │
```

## Які endpoints/функції задіяні

1. `GET /api/public/shows/{slug}` → `public.getShow` → `store.LoadShowBySlug` + `Seats` + `SeatStatuses`
2. (toggle у кошик — лише UI state)
3. `POST /api/public/orders` → `public.createOrder` →
   - `normalizeName`, `normalizeEmail`, `normalizeAttendeeNames`
   - `store.LoadShowBySlug`, `store.Seats`, валідація `seat_ids`
   - `token.NewCode()`
   - `store.CreateOrder(seats, ..., attendeeNames, code, hold)` — атомарно
   - формує `pay_url` через `jarPrefillURL`
4. `GET /api/public/reservations/{code}/status` → `public.reservationStatus` → `store.FindOrderByCode`
5. `POST /webhook` (з моно) → `pay.Processor.processTx` →
   - парсинг код+сума з коментаря
   - `store.FindOrderByCode`
   - validation: `amount >= total_kopecks` (через `MinPrice` чек на seat)
   - `token.QRPayload` для кожної reservation
   - `store.ConfirmOrder` (атомарно встановлює `confirmed_at` і додає `tickets`)
   - `ticket.Render` × N (kожен PDF)
   - `bot.Notifier.SendTicket` × N (якщо `TGChatID != 0`)
   - `email.SendTicketBatchEmail` (один лист, N attachments)

## Опційно: прив'язка Telegram

Якщо у конфізі заданий `BOT_USERNAME` — у success-екрані буде кнопка
`💬 Підключити Telegram`. Натиск → `t.me/<bot>?start=res_<code>` → бот
викликає `LinkOrderToTGChat(code, tgUserID, tgChatID)`. Після оплати
PDF прийде і на email, і в чат.

## Edge cases

| Що | Як обробляється |
|---|---|
| Користувач закрив вкладку | order живе `HOLD` (15 хв), потім sweep ставить cancelled. Якщо встиг оплатити — webhook догребе. |
| Місце забрали між seat map і submit | 409 `seat_taken`; frontend перезавантажує мапу і чистить basket. |
| Одне з N multi-seat місць зайняте | весь order відкочується (атомарно), користувач бачить помилку, обирає інше місце. |
| Webhook не дійшов | `/reconcile` від адміна. Деталі → [[40-flows/reconcile]]. |
| SMTP вимкнено | order все одно confirm-иться; PDF тільки в DB; у логах `WARN buyer has email but no SMTP configured`. |
| Покупець оплатив < ніж total | webhook відкине → status лишається `held` до expires; "переплатив трохи" — OK, ловиться. |

## Frontend файли

- `frontend/src/routes/+page.svelte` — лендінг з плитками
- `frontend/src/routes/event/[slug]/+page.svelte` — picker + form + success + poll
- `frontend/src/lib/api.ts` — типи + `publicApi`

## Дотичне

- [[30-endpoints]] — повний список
- [[40-flows/webhook]] — деталі що відбувається в `pay.Processor`
- [[50-packages/public]] — внутрішня реалізація endpoints
