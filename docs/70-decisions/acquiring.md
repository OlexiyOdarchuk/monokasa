# ADR · Acquiring (Merchant API) поруч з банкою моно

## Контекст

Банка моно достатня для self-host малих заходів — дешево, без бізнес-
кабінету, без фіскалізації, але у моно ліміти (місячний оборот, разові
суми, відсотки). Організатори, що тягнуть «дорослі» заходи, упираються
в ці стелі — їм потрібен справжній еквайринг з invoice'ами, фіскальними
чеками і нормальною звітністю.

Питання: підв'язати monobank Merchant API (`/api/merchant/*`) як
**альтернативний** payment method, не ламаючи jar-flow для малих
організаторів.

## Рішення

**Per-show toggle `shows.payment_method`** = `'jar'` (default, back-compat)
або `'acquiring'`. Admin обирає на створенні шоу. Один інстанс може
тримати обидва методи паралельно — стара афіша на банку, нова на
еквайринг.

### Що змінилось

- **Schema**: `shows.payment_method TEXT DEFAULT 'jar'`, `orders.invoice_id TEXT`
  (зберігаємо id інвойсу для матчу webhook'ом)
- **Config**: `MONO_ACQUIRING_TOKEN` env — окремий токен від
  `MONO_TOKEN` (Personal API). Видається банком окремо для бізнес-
  акаунтів. Порожньо = jar-only режим
- **`pay.Processor`**: нові поля `AcquiringClient` + `BaseURL`, методи
  `CreateAcquiringInvoice` і `HandleAcquiringWebhook`. Lazy-fetch ECDSA
  публічного ключа через `/api/merchant/pubkey` (одноразово, `sync.Once`)
- **Endpoint**: `POST /webhook/acquiring` — 404 коли еквайринг не
  налаштований, що дає operator передбачуване «нема — значить
  вимкнено»
- **Public flow**: `createOrder` після створення ордеру дивиться
  `show.payment_method`; якщо `'acquiring'` — викликає
  `CreateAcquiringInvoice`, повертає `pageURL` як `pay_url` і записує
  invoice id на ордер. Buyer бачить ту саму кнопку «Сплатити» — лендить
  не на jar prefill, а на реальну сторінку інвойсу

### Спільний confirm path

Webhook handler (jar) і `HandleAcquiringWebhook` обидва закінчуються
викликом `deliverConfirmedOrder` — спільний код для:
- `ConfirmOrder` (insert tickets з QR)
- SSE broadcast (`seat_status: sold`)
- audit log (`payment.confirm` / `payment.acquiring`)
- PDF render
- Telegram + email доставка

Тобто бекенд не знає різниці після успіху — тільки кладе різну
audit action для розрізнення в журналі.

## Чому не switch global default

Per-show вибір краще ніж глобальний toggle:
- старі шоу не ламаються, коли admin підключає еквайринг
- різні show'и можуть жити на різних рахунках (банка одна, еквайринг
  інший) — корисно при суборганізаторах
- A/B testing: запустив новий жанр на еквайрингу, лишив звичайні на
  банці

## Trade-offs

| Проблема | Mitigation |
|---|---|
| ECDSA pubkey rotation | Lazy fetch + sticky cache на process lifetime. Ротація → restart процесу |
| Invoice expiry vs hold expiry | `validity` інвойсу = hold duration (`HOLD` env); монобанк інвалідує invoice коли наша бронь expires anyway |
| Buyer payment redirect | `redirectUrl = /event/by-code/<order>` — TODO роут не існує, буде fallback на /event/<slug>; не критично бо paid screen polling сам визначить статус |
| Webhook race vs polling | Idempotent confirm: повторні success-webhook для вже-paid order returns early |

## Реалізація

- **store.SetOrderInvoiceID + FindOrderByInvoiceID** — webhook lookup
- **acquiring.VerifyWebhook + ParseWebhook** з SDK 1.3.0
- **Admin UI**: радіо «🍯 Банка моно» / «💳 Еквайринг» у формі створення
  події. У show editor можна перемикнути на льоту (для існуючих шоу)

## Дотичне

- [[20-data-model]] — `shows.payment_method`, `orders.invoice_id`
- [[30-endpoints]] — `/webhook/acquiring`
- [[50-packages/pay]] — Processor методи
- [[70-decisions/why-monobank-jar]] — попередній ADR (банка як дефолт)
