# 50 · Package: ticket

Рендер PDF A6 з QR-кодом. Single function exported — `Render`.

## Public API

```go
type Renderer struct {
    Secret []byte   // для майбутньої перевірки, поки не використовується тут
    Font   []byte   // вбудовано через embed
}
func (r *Renderer) Render(show Show, seat Seat, name string, qrPayload string) ([]byte, error)
```

## PDF layout

Розмір: A6 (105 × 148 мм). Лежить horizontally чи portrait — залежить
від `landscape` параметра (хардкоднутий портретно зараз).

```
┌─────────────────────────┐
│ <Show.Title>            │
│ <Show.Venue>            │
│ <starts_at fmt>         │
│                         │
│ ┌────────┐  ряд X       │
│ │  QR    │  місце Y     │
│ │        │              │
│ └────────┘  на ім'я:    │
│             <name>      │
│                         │
│ Сканування — на вході.  │
└─────────────────────────┘
```

## Шрифт

`DejaVu Sans` вбудований через `//go:embed assets/DejaVuSans.ttf`.
Підтримує кирилицю — стандартний `helvetica` у gofpdf її не вміє.

## QR

`github.com/skip2/go-qrcode` → PNG → embed у PDF через `fpdf.Image`.

`qrPayload` — це **HMAC-підписаний** рядок, що йде з `token.Coder.QRPayload`. Рендерер його не валідує; це робить сканер.

## Залежності

- `github.com/jung-kurt/gofpdf` — PDF builder pure-Go
- `github.com/skip2/go-qrcode` — QR generator

Обидві pure-Go, без CGo.

## Файли

- `ticket.go` — Renderer + Render
- `assets/DejaVuSans.ttf` — вбудовується

## Дотичне

- [[50-packages/token]] — звідки береться QR payload
- [[40-flows/webhook]] — хто викликає Render
- [[50-packages/web]] — як сканер потім розшифровує
