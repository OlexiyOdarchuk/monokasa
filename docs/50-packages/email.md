# 50 · Package: email

SMTP-доставка PDF-квитків і сповіщень. Без external deps —
`net/smtp` + handmade MIME multipart.

## Public API

```go
type Message struct {
    To, Subject, BodyHTML, BodyText string
    Attachments []Attachment
}
type Attachment struct {
    Filename, ContentType string
    Data []byte
}
type Sender struct {
    Host, Port, User, Pass, From string
    ImplicitTLS bool
}
func (s *Sender) Send(ctx, msg) error
func (s *Sender) SendTicketBatchEmail(ctx, to, name, items, show) error
func (s *Sender) SendCancellationEmail(ctx, to, name, seat, show) error
```

## Чому без зовнішніх SMTP-бібліотек

- `net/smtp` достатній для STARTTLS і LOGIN/PLAIN auth.
- Multipart MIME — це шаблон з ~5 boundary-вставок. Простіше зробити
  вручну і знати, що там, ніж тягнути gomail / mailyak.
- Менше залежностей = менше CVE → менше панік.

## Auto-upgrade STARTTLS

`Send`:
1. `smtp.Dial(host:port)` — plain TCP.
2. Якщо `ImplicitTLS=false` (default) → `EHLO` → перевіряємо `STARTTLS`
   у можливостях → `StartTLS` з `tls.Config{ServerName: host}`.
3. Якщо `ImplicitTLS=true` → `tls.Dial` одразу (для портів 465).
4. `Auth(LOGIN/PLAIN, User, Pass)`.
5. `Mail(From)`, `Rcpt(To)`, `Data` (multipart body).

Помилка на будь-якому кроці → `fmt.Errorf` з контекстом.

## Multipart структура (TicketBatch)

```
multipart/mixed; boundary=X
├─ multipart/alternative; boundary=Y
│  ├─ text/plain  (BodyText)
│  └─ text/html   (BodyHTML)
└─ application/pdf; name="ticket-r3-c5.pdf"  (Attachment 1)
   ...
└─ application/pdf; name="ticket-r3-c6.pdf"  (Attachment N)
```

## Конфіг (ENV)

- `SMTP_HOST`, `SMTP_PORT` (default 587)
- `SMTP_USER`, `SMTP_PASS`
- `SMTP_FROM`
- `SMTP_IMPLICIT_TLS=true` для портів 465

Без `SMTP_HOST` → пакет не створюється; `pay.Processor.Email = nil`;
order все одно confirm-иться, тільки PDF на email не йде.

## Хто викликає

- `pay.Processor` після `ConfirmOrder` — батч N attachments.
- `admin.Handler.cancelNotifier` (через main.go хук) — single attachment-less notification.

## Файли

- `email.go` — Sender, Send, BuildMessage, шаблони
- `email_test.go` — таблиці на BuildMessage (правильні headers, boundary, base64-encoding для не-ASCII subject)

## Дотичне

- [[40-flows/webhook]] — де викликається SendTicketBatchEmail
- [[40-flows/admin]] — де викликається SendCancellationEmail
