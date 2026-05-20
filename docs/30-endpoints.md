# 30 · Endpoints

Усе живе на одному порту (за замовч. `:8093`). Сабпрефікси розділені
по призначенню:

| Префікс | Хто має доступ | Пакет |
|---|---|---|
| `/api/public/*` | будь-хто (web-buyer) | [[50-packages/public]] |
| `/api/admin/*` | RequireAuth (cookie session) | [[50-packages/admin]] |
| `/admin/*` | сторінки логіну | [[50-packages/auth]] |
| `/scan*` | shared-token | [[50-packages/web]] |
| `/posters/*` | будь-хто (read-only) | [[50-packages/posters]] |
| `/webhook` | monobank Personal API | `cmd/app` |
| `/health` | LB | `cmd/app` |
| `/debug/vars` | expvar (Prometheus) | `internal/metrics` |
| `/` і інші статичні | embed SPA | [[50-packages/webui]] |

## Public (без авторизації)

### `GET /api/public/shows`
Список не-archived подій. Відповідь: масив `{slug, title, venue, starts_at, description, poster_url, seats_free, seats_total}`. Використовується лендінгом і Bot.Store.Shows.

### `GET /api/public/shows/{slug}`
Деталі однієї події з seat map. Відповідь: `{slug, title, venue, ..., seats: [{id, row, col, x, y, label, category, price_kopecks, sellable, taken}]}`. `taken=true` = `held` або `sold`.

### `POST /api/public/reservations` (single-seat alias)
```json
{ "slug": "evt-1", "seat_id": 42, "buyer_name": "Олена", "buyer_email": "o@ex.com" }
```
Створює order на 1 місце. Відповідь: `{code, expires_at, pay_url, seat, buyer_name, buyer_email, tg_deep_link?}`.
- 409 `seat_taken` / `seat_not_sellable`
- 400 `invalid_name` / `invalid_email`
- 404 `show_not_found`

### `POST /api/public/orders` (multi-seat)
```json
{
  "slug": "evt-1",
  "seat_ids": [42, 43, 44],
  "buyer_name": "Олена",
  "buyer_email": "o@ex.com",
  "attendee_names": ["Анна", "", "Богдан"]   // optional, length must match seat_ids
}
```
Атомарно тримає всі місця або фейлить.
- 400 `too_many_seats` (>20), `duplicate_seat`, `invalid_input` (mismatched attendee_names)
- 409 `seat_taken` / `seat_not_sellable`
Відповідь: `{code, expires_at, pay_url, total_kopecks, items: [{seat}], buyer_name, buyer_email, tg_deep_link?}`.

### `GET /api/public/reservations/{code}/status`
Полл-ендпоінт для success-екрана. Дивиться в **orders** (бо для multi-seat reservation коди це `code.N`). Відповідь: `{status: "held"|"paid"|"expired"|"cancelled"}`.

## Admin (за `RequireAuth`)

| Метод | Шлях | Що робить |
|---|---|---|
| GET | `/api/admin/me` | поточний адмін |
| GET | `/api/admin/shows` | список подій |
| POST | `/api/admin/shows` | створити; body містить N/M залу і дефолтну ціну |
| GET | `/api/admin/shows/{id}` | одна подія |
| PATCH | `/api/admin/shows/{id}` | title/venue/starts_at/description/poster_url |
| POST | `/api/admin/shows/{id}/archive` | toggle archive |
| GET | `/api/admin/shows/{id}/seats` | full seat list |
| POST | `/api/admin/shows/{id}/seats` | add 1 seat |
| PATCH | `/api/admin/seats` | batch update (drag&drop save) |
| DELETE | `/api/admin/seats/{id}` | видалити |
| GET | `/api/admin/shows/{id}/guests` | бронювання + фільтри |
| GET | `/api/admin/shows/{id}/guests.csv` | експорт |
| POST | `/api/admin/reservations/{id}/cancel` | cascade cancel order; шле сповіщення buyer'у |
| POST | `/api/admin/posters` | multipart upload афіші |

## Auth

| Метод | Шлях | Що робить |
|---|---|---|
| GET | `/admin/login` | HTML-форма |
| POST | `/admin/login` | bcrypt check → cookie |
| POST | `/admin/logout` | drop session |

## Scanner

| Метод | Шлях | Що робить |
|---|---|---|
| GET | `/scan` | HTML scanner UI (камера, jsQR) |
| POST | `/scan/check` | перевірка QR payload, погашення; rate-limit per IP |

## Webhook

`POST /webhook` — monobank Personal API формат. Auth — за `x-effective-amount` + `x-sign` (заголовки моно). Деталі → [[40-flows/webhook]].

## Health / metrics

- `GET /health` → 200 `ok` якщо БД відповідає за 1с, 503 інакше
- `GET /debug/vars` → expvar JSON (квитки видані, помилки webhook, скани)

## Bot-команди

Не HTTP, але теж публічний API:

| Команда | Хто | Що робить |
|---|---|---|
| `/start`, `/help` | всі | афіша |
| `/start res_<code>` | web-buyer | прив'язати чат до order'а |
| `/events`, `/seats` | всі | список подій |
| `/my` | всі | мої бронювання |
| `/stats` | admin TG | продажі активної події |
| `/reconcile [duration]` | admin TG | догребти пропущені webhook'и |
| `/jar` | admin TG | баланс банки моно |

Callback unique-prefixes:
- `show|<slug>` — меню події
- `pick|<slug>` — відкрити seat picker
- `seat|<slug>:<row>:<col>` — toggle місця в кошику
- `done|<slug>` — Завершити вибір
- `clear|<slug>` — очистити кошик
- `cancel|<code>` — скасувати single-seat бронь
- `events|back` — назад до афіші

## Дотичне

- [[40-flows/buyer-web]] — який endpoint що викликає
- [[40-flows/buyer-bot]] — callback-flow
- [[50-packages/public]] / [[50-packages/admin]] — внутрішня реалізація
