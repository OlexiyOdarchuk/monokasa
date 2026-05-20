# 50 · Package: auth

Cookie-сесії для адмін-вебу. bcrypt-хешування паролів, opaque-token
сесії в БД.

## Public API

- `NewHandler(store, secureCookies bool) *Handler`
- `(h *Handler) Register(mux)` — реєструє `/admin/login`, `/admin/logout`
- `(h *Handler) RequireAuth(next http.Handler) http.Handler` — middleware

## Модель

- **Password**: bcrypt cost 12 (~250 мс на check; брутфорс непрактичний).
- **Session token**: 256 бітів `crypto/rand`, base64-encoded; opaque
  (не JWT) — зберігається в `sessions.token`.
- **Cookie**: `monokasa_admin`, HttpOnly, Path=/, SameSite=Lax, MaxAge=30
  днів. `Secure` ставиться:
  - явно, якщо `SECURE_COOKIES=true`
  - або auto, якщо запит прийшов по HTTPS / `X-Forwarded-Proto: https`

## RequireAuth — поведінка

Якщо немає валідної сесії:
- `Accept: text/html` → 303 redirect на `/admin/login` (UX для браузера)
- інакше (API client) → 401 JSON `{error: "unauthorized"}`

Це важливо для SPA: API виклики мають 401 кидати, не редіректити.

## Bootstrap

`cmd/app/main.go` при старті:
1. `FindUserByEmail(ADMIN_EMAIL)` — є?
2. Якщо ні → bcrypt(ADMIN_PASSWORD) → `CreateUser`
3. Лог попередження: "remove ADMIN_PASSWORD from env"

Якщо `users` уже непуста — ENV ігнорується (БД джерело правди).

## Sweep

`store.SweepSessions(ctx)` — видаляє рядки де `expires_at < now`.
Викликається з main.go reminder loop (раз на хвилину). `FindSession`
вже фільтрує expired на читанні, тому sweep — гігієнічна штука.

## CSRF

Не реалізовано окремо. SameSite=Lax + cookie-only auth закривають
більшість сценаріїв. Якщо колись з'являться cross-origin admin POST'и
— треба буде додати CSRF token.

## Файли

- `auth.go` — Handler, login/logout, RequireAuth, cookie helpers
- `auth_test.go` — login/logout/middleware sequences

## Дотичне

- [[40-flows/admin]] — як виглядає вхід зсередини UX
- [[50-packages/admin]] — головний споживач RequireAuth
