# 50 · Package: token

Два різні токени в одному пакеті:
1. **Reservation code** — 8 chars base32, friendly для копіювання
   в коментар платежу.
2. **QR payload** — HMAC-підписаний бінарник, що зашитий у QR на PDF
   і перевіряється на сканері.

## Public API

```go
type Coder struct { secret []byte }
func New(secret []byte) *Coder

func (c *Coder) NewCode() (string, error)              // 8-char base32
func (c *Coder) QRPayload(reservationID, seatID int64) string
func (c *Coder) VerifyQR(payload string) (ResID, SeatID int64, err error)

func ExtractCode(comment string) string                // вільний текст → 8-char
```

## Reservation code

- `crypto/rand` 5 байт → base32 alphabet (`abcdefghijkmnpqrstuvwxyz23456789` — без `loi01v8`, що візуально плутаються).
- Завжди 8 знаків.
- Унікальність — гарантується UNIQUE constraint у БД (при колізії
  `CreateOrder` повертає помилку; перегенеруємо).

## ExtractCode

Покупець у моно може вписати щось нечисте: "abc12345 за квитки".
`ExtractCode` витягає перший континуумий 8-знаковий блок із дозволених
символів. Регексп всередині пакета.

## QR payload

```
payload = base64( resID:8 | seatID:8 | hmac:8 )
hmac    = first 8 bytes of HMAC-SHA256(secret, resID || seatID)
```

- Усього 24 байти → base64 ~32 символи → нормальний QR без overflow.
- 8 байтів HMAC = 64 біти. Брутфорс ~2^63 спроб — непрактичний для
  одноразового сценарію.

## Чому HMAC, не sign

Сканер має той самий secret що й сервер (всі в одному бінарнику). HMAC
дешевший і простіший за asymmetric signing. Цього достатньо: загроза —
"підробка кимось, хто не має нашого secret".

**Не змінюй `TICKET_SECRET` після випуску квитків** — старі QR
перестануть валідуватися.

## Файли

- `token.go` — Coder, NewCode, QRPayload, VerifyQR, ExtractCode
- `token_test.go` — таблиці: roundtrip, tamper-detection, ExtractCode
  з різного сміття

## Дотичне

- [[50-packages/ticket]] — викликає `QRPayload`, малює QR
- [[50-packages/web]] — викликає `VerifyQR` при скануванні
- [[40-flows/webhook]] — приймає `ExtractCode(comment)`
