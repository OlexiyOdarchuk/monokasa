# 50 · Package: pay

Обробник платіжних подій від monobank. Один і той самий код викликається з:
1. `POST /webhook` — звичайний шлях (live moнo).
2. `/reconcile` від адмін-бота — догрібати пропущене (моно statement).

## Public API

- `Processor` struct з полями: `Store`, `Coder`, `Renderer`, `Notifier`, `Email`, `MinPrice`.
- `ServeHTTP(w, r)` — handler для webhook.
- `ProcessTx(ctx, tx) (done bool, err error)` — внутрішня одиниця обробки, експонується reconcile'у.

## Inputs / outputs

```go
type Order struct {
    ID, Code, BuyerName, BuyerEmail, TotalKopecks, ...
}
type OrderItem struct {
    ReservationID int64
    AttendeeName  string  // empty → fallback на BuyerName
    Seat          Seat
}
type Store interface {
    FindOrderByCode(ctx, code) (Order, []OrderItem, error)
    ConfirmOrder(ctx, orderID, qrPayloads map[int64]string) error
}
type Renderer func(show Show, seat Seat, name string, qrPayload string) ([]byte, error)
type Notifier interface { SendTicket(chatID int64, seat Seat, pdf []byte) error }
type EmailDelivery interface {
    SendTicketBatchEmail(ctx, to, name string, items []EmailItem, show Show) error
    SendCancellationEmail(ctx, to, name string, seat Seat, show Show) error
}
```

## Як працює processTx

Детальний крок-за-кроком — [[40-flows/webhook]].

TL;DR:
1. Витягти `code` з comment'а, `amount` з operationAmount.
2. `FindOrderByCode(code)` — якщо не знайдено / закритий → skip.
3. `amount >= sum(seat.Price)` — інакше log warn, skip.
4. Mint QR payloads через `Coder.QRPayload(reservationID, seatID)`.
5. `ConfirmOrder(orderID, qrPayloads)` — атомарно.
6. Render PDF per item.
7. Deliver: TG (per item, якщо `TGChatID != 0`) + Email (one batch, якщо `BuyerEmail`).

## Ідемпотентність

`ConfirmOrder` — джерело правди. Повторний виклик з тим самим order
поверне `ErrAlreadyPaid` (бо `confirmed_at` уже встановлено). Render
+ send викликаються ТІЛЬКИ після успішного `ConfirmOrder`, тому
повторний webhook не дублює PDF.

## Attendee fallback

```go
name := it.AttendeeName
if name == "" {
    name = order.BuyerName
}
pdf, _ := Renderer(show, it.Seat, name, qrs[it.ReservationID])
```

Multi-seat web може заповнити per-ticket attendee; всі інші шляхи (bot,
single-seat web) лишають порожнім → buyer name на PDF.

## Acquiring (Merchant API)

Альтернативний шлях для шоу з `payment_method='acquiring'`. Включається
коли в `Processor` задано `AcquiringClient` (`acquiring.New(token)`) +
`BaseURL`. Без них шоу падає назад на jar при `CreateOrder` (handler-side
fallback у public layer).

Методи:
- `AcquiringEnabled()` — публічний probe для гілки в public handler
- `CreateAcquiringInvoice(ctx, order, items, showTitle)` — викликається
  одразу після `CreateOrderWithDiscount` для acquiring-шоу. Повертає
  `(pageURL, invoiceID)`. `pageURL` стає `pay_url` для buyer'а; `invoiceID`
  пишеться в `orders.invoice_id` для подальшого webhook lookup.
- `HandleAcquiringWebhook(ctx, body, xSign)` — підпис верифікується
  ECDSA-публічним ключем (lazy fetch через `Client.PubKey`, sync.Once
  cache); ParseWebhook + status check; на `InvoiceSuccess` —
  `FindOrderByInvoiceID` + `deliverConfirmedOrder` з audit action
  `payment.acquiring`.

Спільний код: jar webhook (`processTx`) і `HandleAcquiringWebhook`
обидва закінчуються `deliverConfirmedOrder` — той самий confirm +
ticket render + SSE broadcast + audit + delivery. Різниця тільки в
audit action і details (`tx_id` vs `invoice_id`).

Деталі архітектури → [[70-decisions/acquiring]].

## ConfirmFreeOrder

Окрема гілка для 0-kopeck ордерів (100% discount). monobank не випускає
invoice на 0₴, тож і webhook ніколи не прийде. `public.createOrder`
синхронно викликає `ConfirmFreeOrder(code)`, яка валідує `total == 0`,
і виконує той самий `deliverConfirmedOrder` (audit action `payment.free`).
Buyer одразу бачить «оплачено» без редиректу на банк.

## Файли

- `pay.go` — Processor + processTx + acquiring methods + deliverConfirmedOrder
- `pay_test.go` — unit-тести з fake Store, Coder, Renderer, Notifier, Email

## Дотичне

- [[40-flows/webhook]] — повний flow для jar
- [[40-flows/reconcile]] — інший entry point до тієї ж логіки
- [[70-decisions/acquiring]] — чому per-show toggle
- [[50-packages/email]] — як шле батч-листа
- [[50-packages/ticket]] — як рендериться PDF
