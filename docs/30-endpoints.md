# 30 · Endpoints

Усе живе на одному порту (за замовч. `:8093`). Сабпрефікси розділені
по призначенню:

| Префікс | Хто має доступ | Пакет |
|---|---|---|
| `/api/public/*` | будь-хто (web-buyer) | [[50-packages/public]] |
| `/api/admin/*` | RequireAuth (cookie session) | [[50-packages/admin]] |
| `/admin/*` | сторінки логіну | [[50-packages/auth]] |
| `/scan*` | shared-token cookie АБО Telegram WebApp init data | [[50-packages/web]] |
| `/posters/*` | будь-хто (read-only) | [[50-packages/posters]] |
| `/webhook` | monobank Personal API (jar) | `cmd/app` |
| `/webhook/acquiring` | monobank Merchant API (signed) | `cmd/app` + [[50-packages/pay]] |
| `/health` | LB | `cmd/app` |
| `/debug/vars` | expvar (Prometheus) | `internal/metrics` |
| `/event/<slug>` | OG-wrap → embed SPA | `cmd/app` + [[50-packages/og]] |
| `/` і інші статичні | embed SPA | [[50-packages/webui]] |

## Public (без авторизації)

### `GET /api/public/shows`
Список не-archived подій. Відповідь: масив `{slug, title, venue, starts_at, description, poster_url, seats_free, seats_total, kind, sessions?}`. `kind` — `"seated"` або `"ga"`. Якщо кілька подій мають однаковий `session_group`, вони колапсуються в один елемент із масивом `sessions: [{slug, starts_at, seats_free}]` і canonical-ом стає найближча. Використовується лендінгом і Bot.Store.Shows.

### `GET /api/public/shows/{slug}`
Деталі однієї події з seat map. Відповідь: `{slug, title, venue, ..., kind, seats: [{id, row, col, x, y, label, category, price_kopecks, sellable, taken}], categories: [...]}`. `taken=true` = `held` або `sold`.

Якщо подія належить до multi-session серії — додається `siblings: [{slug, starts_at, seats_free}]` зі списком ІНШИХ дат тієї ж серії.

Для GA (kind="ga") `seats[]` повертається **порожнім** — концептуально місць немає, є лише пул. Замість того додаються:
- `ga_capacity` — оригінальний розмір пулу
- `ga_price_kopecks` — uniform-ціна квитка
- `ga_free` — скільки вільних квитків зараз

### `POST /api/public/reservations` (single-seat alias)
```json
{ "slug": "evt-1", "seat_id": 42, "buyer_name": "Олена", "buyer_email": "o@ex.com" }
```
Створює order на 1 місце. Відповідь: `{code, expires_at, pay_url, seat, buyer_name, buyer_email, tg_deep_link?}`.
- 409 `seat_taken` / `seat_not_sellable`
- 400 `invalid_name` / `invalid_email`
- 404 `show_not_found`

### `POST /api/public/orders` (multi-seat або GA quantity)
```json
{
  "slug": "evt-1",
  "seat_ids": [42, 43, 44],
  "buyer_name": "Олена",
  "buyer_email": "o@ex.com",
  "attendee_names": ["Анна", "", "Богдан"]   // optional, length must match seat_ids
}
```
Опційно у body: `"discount_code": "EARLYBIRD"` — backend валідіює і apply'ить atomic'ом разом з створенням ордеру. Помилки: 400 `discount_not_found` / `discount_inactive` / `discount_expired` / `discount_used_up`. Відповідь додатково містить `discount_code` і `discount_kopecks` коли застосовано.

Для GA — `seat_ids` опускається, додається `"quantity": 3`:
```json
{ "slug": "standup", "quantity": 3, "buyer_name": "Олена", "buyer_email": "o@ex.com" }
```
Сервер сам обере 3 вільних квитки з пулу. Передавати обидва одночасно — 400.

Атомарно тримає всі місця або фейлить.
- 400 `too_many_seats` (>20), `duplicate_seat`, `invalid_input` (mismatched attendee_names, обидва seat_ids+quantity, GA з seat_ids, seated з quantity)
- 409 `seat_taken` / `seat_not_sellable` / `not_enough_seats` (GA: вільних менше ніж quantity)
Відповідь: `{code, expires_at, pay_url, total_kopecks, items: [{seat}], buyer_name, buyer_email, tg_deep_link?}`. Для GA `items[].seat` має `category="GA"` і `col` = номер квитка.

### `GET /api/public/reservations/{code}/status`
Полл-ендпоінт для success-екрана. Дивиться в **orders** (бо для multi-seat reservation коди це `code.N`). Відповідь: `{status: "held"|"paid"|"expired"|"cancelled"}`.

### `GET /api/public/shows/{slug}/events`
Server-Sent Events stream із seat-status оновленнями для однієї події.
Фронтенд (`/event/<slug>`) тримає відкритим, щоб мапа оновлювалась без
polling'у. Кожне повідомлення — `data: {"type":"seat_status","seat_id":N,"status":"free|held|sold"}\n\n`.
Keep-alive comment `: ping\n\n` кожні 25 секунд. 503 `realtime_disabled`
якщо hub не сконфігурований. Деталі → [[50-packages/realtime]].

### `POST /api/public/waitlist`
```json
{ "slug": "evt-1", "email": "buyer@example.com" }
```
Записує buyer у чергу очікування на події, де всі місця зайняті. Сервер дедуплікує по `UNIQUE(show_id, email)` — повторні запити безпечні. Відповідь: `{status:"ok", already_notified}`. Коли seat звільниться (sweep expired holds, admin cancel), бекенд відсилає email перших N waitлістерів (cap=5 за один батч).

### `GET /api/public/organizer`
Single-row профіль для сторінки `/about`. Завжди 200, навіть якщо все порожнє — порожні поля = сигнал "не налаштовано". Відповідь: `{name, bio, contact_email, phone, website_url, telegram_url, instagram_url, facebook_url, logo_url}`.

### Buyer "Мої квитки" (magic-link auth)

| Метод | Шлях | Що робить |
|---|---|---|
| POST | `/api/public/login/request` | `{email}` → шле magic-link (або 200 `status:"logged"` якщо SMTP не сконфігурований — лінк у логах) |
| GET | `/api/public/login/consume?token=` | Burn token + Set-Cookie + 303 на `/my` (або `/my?error=…` при невдачі) |
| POST | `/api/public/login/logout` | Видалити cookie + сесію |
| GET | `/api/public/my` | whoami → `{email}` або 401 |
| GET | `/api/public/my/tickets` | Список замовлень з QR payloads (paid only) |

Magic-link URL вказує **прямо на `/api/public/login/consume`** (не на
`/my?token=…`) — щоб браузер нативно обробив 303 з Set-Cookie. Fetch-
based підхід раніше ламав cookie в деяких браузерах. Деталі →
[[40-flows/buyer-my-tickets]].

## Admin (за `RequireAuth`)

| Метод | Шлях | Що робить |
|---|---|---|
| GET | `/api/admin/me` | поточний адмін |
| GET | `/api/admin/shows` | список подій |
| POST | `/api/admin/shows` | створити; для seated body містить rows/cols+price; для GA `kind:"ga", ga_capacity:N, price_kopecks:P` |
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
| GET | `/api/admin/shows/{id}/categories` | список цінових категорій |
| POST | `/api/admin/shows/{id}/categories` | upsert tier (creates or updates by name); cascade-syncs seat prices |
| DELETE | `/api/admin/categories/{id}` | видалити tier (не торкає seats.category labels) |
| GET | `/api/admin/shows/{id}/poster-qr.png` | PNG QR-код на /event/<slug> для друкованих афіш |
| PATCH | `/api/admin/shows/{id}/ga-capacity` | `{ga_capacity}` — grow/shrink GA пул. Shrink → 409 `seats_booked` коли хвостові квитки вже продані |
| GET | `/api/admin/organizer` | single-row профіль (name/bio/socials) |
| PUT | `/api/admin/organizer` | overwrite профіль (всі поля опційні; bio ≤2000 chars) |
| GET | `/api/admin/analytics?days=N` | агрегати: daily_sales[], total_*, конверсія, per_show[]; days∈[1,365] |
| GET | `/api/admin/discounts` | список промокодів (newest first) |
| POST | `/api/admin/discounts` | створити; `{code, kind:'percent'\|'fixed', value, scope:'order'\|'ticket', max_uses, expires_at?, active}` |
| PATCH | `/api/admin/discounts/{id}` | оновити kind/value/scope/max_uses/expires_at/active (code immutable) |
| DELETE | `/api/admin/discounts/{id}` | видалити (історичні orders зберігають назву) |

## Auth

| Метод | Шлях | Що робить |
|---|---|---|
| GET | `/admin/login` | HTML-форма |
| POST | `/admin/login` | bcrypt check → cookie |
| POST | `/admin/logout` | drop session |

## Scanner

| Метод | Шлях | Що робить |
|---|---|---|
| GET | `/scan` | HTML scanner UI (камера, jsQR). Password gate якщо `SCANNER_TOKEN` set |
| GET | `/scan?tg=1` | Те саме, але одразу серветь сторінку без password gate — для Telegram WebApp кнопки бота. Auth все одно перевіряється на `/scan/check` |
| POST | `/scan/check` | Перевірка QR payload + погашення; rate-limit per IP. Auth: cookie з SCANNER_TOKEN, `X-Scanner-Token` header, **або** `X-Telegram-Init-Data` (server verify через bot token + admin allow-list) |

## Webhook

- `POST /webhook` — monobank **Personal API** (jar transactions). Auth за `x-effective-amount` + `x-sign` (заголовки моно). Деталі → [[40-flows/webhook]].
- `POST /webhook/acquiring` — monobank **Merchant API** (acquiring invoices). Auth за ECDSA `X-Sign` header проти merchant pubkey (`/api/merchant/pubkey`, lazy-cached). 404 коли `MONO_ACQUIRING_TOKEN` не set. Деталі → [[70-decisions/acquiring]].

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
