# 40 · Flow: reconcile — рятувальна сітка

## Навіщо

Webhook moнo може не дійти. Причини: downtime, неправильно
зареєстрований URL, мережа, рестарт під час події. Без reconcile —
покупець заплатив, але PDF не отримав; адмін бачить "held", покупець
бачить "очікую оплати" в success-екрані.

Reconcile = "пройди по виписці моно за останній X-час, кожну транзакцію
прожени через ту саму гілку, що й webhook".

## Хто може запустити

`/reconcile [duration]` від `ADMIN_TG_ID` у боті (TG, не веб).
Дефолт duration — 24h. Можна `/reconcile 48h`, `/reconcile 7d`. Максимум
`720h` (30 днів — moнo не віддає старіше).

Потрібен `MONO_TOKEN` в ENV. Без нього `/reconcile` каже "недоступний".

## Послідовність

```
admin           bot               reconciler        monobank          pay.Processor
  │              │                    │                 │                   │
  │ /reconcile   │                    │                 │                   │
  │─────────────►│ handleReconcile    │                 │                   │
  │              │ start ack          │                 │                   │
  │◄─────────────│ "⏳ Шукаю..."      │                 │                   │
  │              │                    │                 │                   │
  │              │ Reconcile(24h) ───►│                 │                   │
  │              │                    │ /personal/      │                   │
  │              │                    │   client-info ─►│                   │
  │              │                    │ /personal/      │                   │
  │              │                    │   statement ───►│                   │
  │              │                    │   (per account, │                   │
  │              │                    │    rate-limited)│                   │
  │              │                    │                 │                   │
  │              │                    │ for each tx:    │                   │
  │              │                    │   ProcessTx ───────────────────────►│
  │              │                    │     (та сама    │                   │
  │              │                    │     гілка, що   │                   │
  │              │                    │     й webhook)  │                   │
  │              │                    │                 │                   │ ConfirmOrder
  │              │                    │                 │                   │ → PDF → send
  │              │                    │                 │                   │ (АБО ErrAlreadyPaid
  │              │                    │                 │                   │  → skip)
  │              │ progress edits     │                 │                   │
  │◄─────────────│ "⏳ Знайдено N..."│                 │                   │
  │              │                    │                 │                   │
  │◄─────────────│ "✅ Готово,        │                 │                   │
  │              │     N сканів, M    │                 │                   │
  │              │     нових квитків" │                 │                   │
```

## Чому це безпечно

`pay.Processor.processTx` (той самий, що й webhook):
- `ConfirmOrder` ідемпотентний → повторно не confirm-ить.
- Якщо order вже confirmed → returns ErrAlreadyPaid, ми просто skip.
- Render+send відбувається ТІЛЬКИ якщо ConfirmOrder реально щось змінив.

Тобто: reconcile після успішного webhook'а не висилає другий PDF, а
тихо пропускає всі вже-confirmed транзакції.

## Rate-limit moнo

`/personal/statement` лімітований **1 запит / 60 с / account**. Якщо у
`ClientInfo` кілька рахунків + банок — їх треба обходити з паузою.
Reconciler має `KeyedLimiter` per-account, який автоматично чекає.

Тому `/reconcile 24h` — це 1 запит на кожен акаунт + 1 на банку → ~3
запити → 3 хвилини якщо є 3 endpoints. Прогрес едітується в живому
повідомленні через `progress` callback.

## Структура

```go
package mono  // у go-monobank-sdk
type Reconciler struct {
    Client    *Client
    Limiter   *KeyedLimiter
    Processor pay.Processor   // pay.ProcessTx fn
}
type ReconcileResult struct {
    Scanned int  // переглянуто транзакцій
    Matched int  // нових тікетів видано
}
```

## Файли

- `internal/bot/bot.go::handleReconcile` — Telegram entry point
- `cmd/app/main.go` — wiring, створення Reconciler
- `pay.Processor.ProcessTx` — fn, що передається в reconcile
- `go-monobank-sdk` (external module, у нас v1.3.0+)

## Що НЕ робить reconcile

- Не повертає кошти (refund — руками в моно)
- Не редагує order/seat дані
- Не торкається webhook-підписки
- Не виправляє order'и без транзакції (без webhook + без виписки = немає
  чим матчити)

## Дотичне

- [[webhook]] — основна гілка обробки
- [[50-packages/pay]] — Processor як єдина точка confirm
