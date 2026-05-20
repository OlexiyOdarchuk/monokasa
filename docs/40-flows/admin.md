# 40 · Flow: адмін

## Bootstrap першого адміна

Перший вхід — через ENV:

```sh
ADMIN_EMAIL=you@example.com
ADMIN_PASSWORD=<strong>
```

При старті `cmd/app/main.go` бачить порожню `users` → bcrypt-ить пароль
(cost 12) → INSERT → ВИЛОГИЄ попередження "remove ADMIN_PASSWORD from
ENV". Подальші перезапуски ці змінні **ігнорують** — БД джерело правди.

## Логін

1. `GET /admin/login` → HTML-форма (рендериться auth-пакетом)
2. `POST /admin/login` з email+password → `auth.HandleLogin`:
   - `store.FindUserByEmail` → bcrypt compare
   - `store.CreateSession(userID, token, expires)` — 256-bit random
   - Set-Cookie `monokasa_admin=<token>; HttpOnly; Path=/; SameSite=Lax; MaxAge=30d`
3. 303 → `/admin`

## Звичайні дії

Усі під `/api/admin/*` → `auth.RequireAuth` middleware:
- читає cookie → `FindSession` → 401/303 якщо немає/протермінований
- intent: текстова відповідь 401 JSON для API, 303 redirect для HTML
  (через `Accept: text/html` differentiator)

### Створити подію

```
POST /api/admin/shows
{ "title": "...", "venue": "...", "starts_at": "2026-...",
  "rows": 8, "cols": 12, "price_kopecks": 25000 }
```
→ `admin.createShow`:
1. `Slugify(title)` — ASCII-safe, кириличні title fall back на `show-<id>`
2. `store.CreateShow(show, rows, cols, price)` створює show + N×M місць
3. 201 з повним об'єктом

### Редактор залу (drag&drop)

Frontend: `frontend/src/routes/admin/shows/[id]/layout/+page.svelte`.

- завантажує `GET /api/admin/shows/{id}/seats`
- DnD рухає `(x, y)` місць; ціна / категорія / sellable редагуються
  inline
- зміни накопичуються в `SvelteSet<seat_id>` "dirty"
- click "Save" → `PATCH /api/admin/seats` з `{seats: [SeatPatch]}` →
  `store.UpdateSeats` (єдиний tx)
- toast "Збережено" або "Помилка"

### Постер

`POST /api/admin/posters` — multipart:
- `posters.HandleUpload`:
  - `http.MaxBytesReader(5MB)` cap
  - `DetectContentType` валідація: jpg/png/webp
  - `rand 16 bytes → 32 hex` filename
  - запис у `POSTERS_DIR/<hex>.<ext>`
  - відповідь: `{url: "/posters/<hex>.<ext>"}`
- адмін копіює URL у `poster_url` поля show через `PATCH /api/admin/shows/{id}`
- `GET /posters/<filename>` — `posters.HandleServe`, path-traversal-safe,
  static immutable cache

### Список гостей події

`GET /api/admin/shows/{id}/guests?status=paid|held|cancelled` —
`admin.listGuests` (`store.GuestsForShow`). Може експортуватися у CSV
(`guests.csv`).

### Force-cancel

`POST /api/admin/reservations/{id}/cancel` → `admin.cancelReservation`:
1. `store.AdminCancelReservation(reservationID)` — cascade на ВЕСЬ
   order; всі rows + order row отримують `cancelled_at`.
2. Викликається `cancelNotifier` (callback, зареєстрований у main.go):
   - якщо `TGChatID != 0` → `bot.SendCancellation`
   - якщо `BuyerEmail != ""` і SMTP → `email.SendCancellationEmail`
3. Помилки доставки тільки логуються — DB вже консистентна.

### Archive

`POST /api/admin/shows/{id}/archive` — toggle `archived_at`. Архівні
ховаються з landing і боту (Bot.Store.Shows і public.listShows
фільтрують `ArchivedAt != nil`).

## Безпека

- bcrypt cost 12 → ~250 мс на check; брутфорс непрактичний.
- sessions — opaque token, не JWT; видалення сесії = одразу logout
  (немає кешів).
- SECURE_COOKIES=true для прода (X-Forwarded-Proto auto-detect також).
- CSRF: SameSite=Lax + cookies = OK для більшості випадків (немає
  cross-origin admin POST'ів).

## Файли

- `internal/admin/admin.go` — handlers
- `internal/auth/auth.go` — bcrypt, sessions, RequireAuth
- `internal/posters/posters.go` — upload
- `frontend/src/routes/admin/` — UI

## Дотичне

- [[50-packages/admin]] — структура пакета
- [[50-packages/auth]] — сесійна модель
- [[30-endpoints]] — повний реєстр endpoints
