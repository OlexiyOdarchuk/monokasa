# 50 · Package: public

Анонімний API для web-покупця. Шлях `/api/public/*`, **без auth**.

## Endpoints

(повний реєстр → [[30-endpoints]])

- `GET /api/public/shows` — список не-archived з лічильниками вільно/всього
- `GET /api/public/shows/{slug}` — деталі + seat map
- `POST /api/public/reservations` — single-seat alias (бекап old client)
- `POST /api/public/orders` — multi-seat (1..20), приймає опційні
  `attendee_names[]`
- `GET /api/public/reservations/{code}/status` — поллінг; читає
  **orders**, не reservations (бо multi-seat reservation codes — `code.N`)

## Залежності

```go
type Handler struct {
    st          *store.Store   // безпосередньо, без інтерфейсу
    coder       *token.Coder   // NewCode()
    jarLink     string
    hold        time.Duration
    priceMin    int64
    botUsername string          // optional, для tg_deep_link у відповіді
}
```

## Сanity-check'и в `createOrder`

- `len(seat_ids) <= 20` — soft cap
- унікальність seat_id (`duplicate_seat`)
- кожен seat існує (`seat_not_found`) і `sellable`
- кожен seat.PriceKopecks >= `priceMin` (sanity, не для buyer-facing
  помилки)
- `attendee_names` опційний; якщо є — довжина має дорівнювати `seat_ids`
- normalizeName / normalizeEmail — 2..60 рун, валідний email

## Error envelope

```json
{ "error": "<machine_code>", "detail": "<human, optional>" }
```

Machine codes: `invalid_input`, `invalid_name`, `invalid_email`,
`invalid_attendee_name`, `seat_taken`, `seat_not_sellable`,
`seat_not_found`, `duplicate_seat`, `too_many_seats`, `show_not_found`,
`not_found`, `internal`.

## Файли

- `public.go` — Handler, всі endpoints, нормалізатори, error helpers
- `public_test.go` — httptest на справжньому Store

## Дотичне

- [[40-flows/buyer-web]] — як endpoint'и складаються у flow
- [[50-packages/store]] — куди йдуть виклики
