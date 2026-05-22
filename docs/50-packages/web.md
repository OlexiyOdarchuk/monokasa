# 50 · Package: web

Сканер на вході. `/scan` HTML-сторінка + `/scan/check` POST-ендпоінт.

## Public API

```go
type Scanner struct {
    store        Store
    coder        *token.Coder
    sharedToken  string         // якщо порожній — без gate
    metrics      *Metrics
    rateLimiter  *PerIPLimiter
}

func New(opts Options) *Scanner
func (s *Scanner) Register(mux *http.ServeMux)
```

## `/scan` GET — HTML

- Якщо `SCANNER_TOKEN` не порожній і у запиті немає валідної scanner-cookie
  → показуємо HTML-форму "введи пароль".
- Якщо токен валідний → видаємо HttpOnly cookie (`Path=/scan`, MaxAge 12h)
  і одразу повертаємо сторінку сканера.
- Сторінка сканера: монтує `jsQR` через CDN, бере камеру через
  `getUserMedia`, на кожен фрейм пробує декодувати; знайшов → fetch
  `/scan/check` з payload.

## `/scan/check` POST

```json
{ "payload": "<base64 from QR>" }
```

1. `rateLimiter` per-IP: 10 спроб/с, бан 60 с.
2. Перевірка cookie (якщо `SCANNER_TOKEN` заданий).
3. `token.Coder.VerifyQR(payload)` → (`resID`, `seatID`, err)
   - err → `{status: "invalid"}` + metrics.
4. `store.UseTicket(payload)`:
   - `ErrTicketNotFound` → `invalid`
   - `ErrTicketUsed` → `{status: "used", used_at: ..., name, seat}`
   - інакше → `{status: "ok", name, seat}` (UPDATE used_at у транзакції)
5. JSON відповідь сторінці.

## Status responses → UI

- `ok` — зелений + м'який акорд → пускати
- `used` — оранжевий + одиничний тон + вібрація → не пускати, з'ясовувати
- `invalid` — червоний + різкий тон → підробка / чужий QR

## Anti-replay

`tickets.used_at` UPDATE — атомарний. Раз ticket used, всі наступні
скани = `used`. Бан-логіка на IP — захист від брутфорсу через підбір
QR.

## Auth paths

`Scanner.authOK` приймає 3 шляхи (any одного достатньо):

1. **Cookie `monokasa_scan`** з shared `SCANNER_TOKEN` — set після успіху
   на password form `GET /scan`. Persistent (configured TTL).
2. **Header `X-Scanner-Token`** — те саме shared token, для CLI-fetch
   або custom integrations. Constant-time compare.
3. **Header `X-Telegram-Init-Data`** — підписаний blob з Telegram Mini
   App. `VerifyTelegramInitData(initData, botToken)` відтворює HMAC-
   SHA256 ланцюжок (`secret = HMAC("WebAppData", botToken)`,
   `hash = HMAC(secret, sortedFields)`), читає `user.id`, перевіряє у
   admin allow-list (`EnableTelegramWebApp(botToken, []adminTGID)`).

Bot button у `/start` для `ADMIN_TG_ID` лінкує на `/scan?tg=1` як
WebApp. `?tg=1` query вмикає shortcut serving scanner page без password
gate; auth все одно перевіряється на `/scan/check`. Деталі →
[[70-decisions/scanner-tg-webapp]].

## Файли

- `scan.go` — Scanner, обидва handler'и
- `page.go` — embed HTML (підвантажує telegram-web-app.js)
- `tgwebapp.go` — `VerifyTelegramInitData` + unit tests
- `ratelimit.go` — Limiter, ClientIP, GC (експортовано для public/auth)
- `scan_test.go` — flow happy/used/invalid + rate-limit
- `tgwebapp_test.go` — signature ok/mismatch/missing-hash/missing-user

## Чому HTML, не SPA

Сторінка дуже проста (камера + 1 виклик API), Vue/React тут зайве. JS
mini, без бандлера, можна копіювати на коліні і дебажити.

## Дотичне

- [[50-packages/token]] — VerifyQR
- [[50-packages/store]] — UseTicket
