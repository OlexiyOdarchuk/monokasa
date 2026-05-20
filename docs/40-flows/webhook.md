# 40 · Flow: webhook → confirm → доставка

## Як monobank потрапляє на нас

Покупець платить на банку моно. Моно шле `POST https://<BASE_URL>/webhook`
з тілом виду:

```json
{
  "type": "StatementItem",
  "data": {
    "statementItem": {
      "id": "...",
      "time": 17...,
      "amount": 25000,            // копійки, ВЖЕ враховує комісію
      "operationAmount": 25000,
      "description": "Олена Петренко",
      "comment": "abc12345"       // це наш order code
    }
  }
}
```

## Хто це обробляє

`cmd/app/main.go` реєструє `mux.Handle("/webhook", payProc)`. Сам `pay.Processor`:

```go
type Processor struct {
    Store    Store        // FindOrderByCode, ConfirmOrder
    Coder    Coder        // QRPayload
    Renderer Renderer     // (show, seat, name, qr) → PDF
    Notifier Notifier     // bot.SendTicket
    Email    EmailDelivery
    MinPrice int64
}
```

## Покрокова логіка processTx

```
1. parseCommentCode(comment) → "abc12345"  (8-char base32; нормалізація з вільного коментаря)
2. amount, _ = parseAmount(operationAmount)  → копійки
3. order, items, err := Store.FindOrderByCode(ctx, code)
   - ErrCodeNotFound → log і return (нічого не робимо)
   - ErrAlreadyClosed → log і return
4. amount >= sum(seat.Price for item in items)? якщо ні → log warn і return
5. qrs := map[reservationID]string
   for each item:
     qrs[item.ReservationID] = Coder.QRPayload(item.ReservationID, item.Seat.ID)
6. Store.ConfirmOrder(orderID, qrs)
   - атомарно ставить confirmed_at на order + усі дочірні reservations
   - INSERT INTO tickets для кожної reservation з її qr_payload
   - ідемпотент: повторний виклик не дублює, повертає ErrAlreadyPaid
7. for each item: render PDF
   - Renderer(show, item.Seat, attendeeName_or_buyerName, qrs[item.ReservationID])
   - якщо рендер фейлить — skip цей PDF, continue (інші не блокуються)
8. (опційно) Telegram delivery:
   if order.TGChatID != 0:
     for each (item, pdf): Notifier.SendTicket(TGChatID, item.Seat, pdf)
9. (опційно) Email delivery:
   if order.BuyerEmail != "" and Email != nil:
     Email.SendTicketBatchEmail(BuyerEmail, BuyerName, items+pdfs, show)
   (помилка SMTP не блокує — order вже confirmed)
```

## Ідемпотентність

Webhook moно може дійти ДВА рази (retry на 5xx). Безпечно:

- `Store.ConfirmOrder` усередині транзакції перевіряє `confirmed_at IS NULL`
  на ордері; якщо встановлено — повертає `ErrAlreadyPaid` без змін.
- Повторний виклик `Confirm` пакетом тригерить ErrAlreadyPaid → `processTx`
  логує і повертає `done=false, nil`.
- Чи буде PDF другий раз надіслано? Ні: на повторному виклику ConfirmOrder
  падає → код далі по гілці не йде → SendTicket/Email не викликаються.

## Чому amount check — per-order, не per-seat

Покупець може оплатити одним переказом N місць (multi-seat). `amount` —
сума всього платежу. Перевіряємо `total = sum(seat.Price)`. Менше →
відхиляємо. Більше — приймаємо (переплатив трохи — це нормально).

## Що з minPrice

`MinPrice` залишився з часів single-seat для sanity check'а: якщо одне
місце дешевше ніж `MinPrice` копійок — це підозріло (місце з нульовою
ціною?), processor відхиляє. Перевіряється per-seat у public.createOrder
ще на стадії reservation, не в webhook.

## Що буде, якщо webhook не дійшов

Moнo retries не назавжди. Якщо мережа лягла, наш бекенд був down,
DNS глючив — webhook загубиться.

Тоді: `/reconcile` від адміна → [[reconcile]].

## Хто реєструє webhook URL у моно

Якщо в .env є `MONO_TOKEN` + `WEBHOOK_URL` — `cmd/app/main.go` при
старті викликає `POST https://api.monobank.ua/personal/webhook`. Мono
GET-пінгає URL перед тим, як прийняти підписку, тому це робиться з
exponential-backoff retry (5 спроб, ~30 с сумарно) — щоб пережити
повільне піднімання TLS-фронту після `docker compose up`.

Без `MONO_TOKEN` — реєструй вручну (curl у README).

## Файли

- `internal/pay/pay.go` — Processor, парсери, доставка
- `cmd/app/main.go` — wiring адаптерів, retry-реєстрація вебхука
- `internal/store/store.go::ConfirmOrder` — атомарна частина

## Дотичне

- [[40-flows/reconcile]] — як догребти пропущене
- [[50-packages/pay]] — структура пакета
- [[50-packages/email]] — як шле батч-листа
