# 20 · Модель даних

## Сутності

```
   ┌───────┐     ┌──────┐
   │ shows │1───*│seats │
   └───┬───┘     └──┬───┘
       │            │
       │            │
       │       ┌────┴─────────┐
       │       │              │
       │  ┌────▼──────────┐   │
       └──┤ reservations  │   │
          │  (order_id ──┐│   │ ← seat_id 1:1
          └──┬───────────┘│   │
             │            │   │
             │       ┌────▼───▼─┐
             │       │  orders  │ ← (1) order ─→ (1..N) reservations
             │       └──────────┘    one pay code covers all
             │
             │
        ┌────▼─────┐
        │ tickets  │ ← created on Confirm, points at reservation, 1:1
        └──────────┘
```

Окремо: `users` (адмін-логіни) + `sessions` (auth-токени) + `organizer` (single-row профіль для /about) + `discount_codes` (промокоди) + `waiting_list` (буфер email'ів для sold-out подій) + `seat_categories` (pricing tiers).

## Кожна таблиця

### shows
| col | type | заміт |
|---|---|---|
| id | INTEGER PK | |
| slug | TEXT UNIQUE | URL-handle, ASCII, fallback `show-<id>` для кириличних title |
| title | TEXT | |
| venue | TEXT | |
| starts_at | INTEGER | unix sec |
| description | TEXT | free-form |
| poster_url | TEXT | відносний `/posters/<32hex>.jpg` або external https:// |
| created_at | INTEGER | |
| archived_at | INTEGER | nullable; non-null = ховаємо з landing і бота |
| kind | TEXT | `"seated"` (default) або `"ga"`. GA шоу = virtual-seat pool, без мапи залу |
| ga_capacity | INTEGER | для GA — розмір пулу. Мутабельний через `PATCH /shows/{id}/ga-capacity` (grow → append seats, shrink → refuse якщо хвостові продані) |
| session_group | TEXT | мітка для multi-session (та сама вистава на кілька дат). Порожньо = разова подія |
| payment_method | TEXT | `"jar"` (default) → moonbank jar prefill URL; `"acquiring"` → реальний merchant invoice. Per-show toggle |

### seats
| col | type | заміт |
|---|---|---|
| id, show_id, row, col | | UNIQUE(show_id, row, col) |
| x, y | REAL | canvas координати для drag&drop |
| label | TEXT | "" → "Ряд R · місце C" |
| category | TEXT | "vip", "balcony" тощо; впливає на колір. Для GA shows завжди `"GA"` |
| price_kopecks | INTEGER | per-seat ціна |
| sellable | INTEGER | 0 = проходи / технічні |

Для GA shows seats створюються як virtual pool: row=1, col=1..N, всі sellable=1.
Уся логіка hold/sold/refund/scan не знає різниці між GA і seated — це просто seats.

### orders
| col | type | заміт |
|---|---|---|
| id, code | INT, TEXT UNIQUE | code = 8-char base32 (a-z + 2-7) |
| buyer_name, buyer_email | | |
| tg_user_id, tg_chat_id | | 0 для web-only |
| total_kopecks | | post-discount сума до оплати |
| discount_code | TEXT default '' | snapshot назви промокоду (зберігається після `DELETE FROM discount_codes`) |
| discount_kopecks | INT default 0 | скільки відняли від суми місць |
| invoice_id | TEXT default '' | monobank acquiring invoice id (тільки для `payment_method='acquiring'`); порожньо для jar-flow |
| created_at, expires_at | | |
| confirmed_at, cancelled_at, reminded_at | nullable | |
| refunded_at | nullable | bookkeeping, не впливає на seat статус |

### reservations
| col | type | заміт |
|---|---|---|
| id, order_id, seat_id, tg_user_id, tg_chat_id | | |
| buyer_name, buyer_email | | дублюється з order для legacy queries |
| attendee_name | TEXT default '' | per-ticket ім'я; '' = fallback на BuyerName |
| code | TEXT UNIQUE | `code` для single-seat, `code.1`/`code.2`/… для multi |
| created_at, expires_at, confirmed_at, cancelled_at, reminded_at | | |

### tickets
| col | type | заміт |
|---|---|---|
| id, reservation_id | | один tкт per reservation |
| qr_payload | TEXT | HMAC-підписаний; см. [[50-packages/token]] |
| issued_at, used_at | | non-null used_at → "вже використано" на сканері |

### users
- bcrypt cost 12, password_hash

### sessions
- 256-bit opaque token, MaxAge 30 днів

### buyer_login_tokens
- Одноразовий 256-bit hex token emailed buyer'у для magic-link login
- email (lowercase), expires_at (TTL 15 хв), used_at (один shot)

### buyer_sessions
- 256-bit opaque cookie token для довгоживучої buyer-сесії
- email (lowercase), MaxAge 30 днів
- Окремо від `sessions` (admin) — щоб один браузер міг бути одночасно
  admin'ом і buyer'ом

### seat_categories
- pricing tier per show: `(show_id, name UNIQUE)` + `color`, `price_kopecks`, `sort_order`
- `seats.category` (string) join'иться до `name`; upsert tier-у каскадно
  оновлює `seats.price_kopecks` всіх місць з цією назвою

### discount_codes
- `code TEXT UNIQUE COLLATE NOCASE` (EARLY/early однакові)
- `kind` ∈ {`percent`, `fixed`} + `value` (1..100 / kopecks)
- `scope` ∈ {`order`, `ticket`} — `ticket` обмежує знижку ціною одного
  (найдешевшого) квитка щоб «100% comp» не злив весь кошик
- `max_uses` (0 = ∞), `used_count` атомарно інкрементиться у тій самій
  tx що CreateOrder — два конкурентних buyer'и не можуть оба видоїти
  ост анню знижку
- `expires_at` (nullable), `active` toggle для emergency disable

### waiting_list
- `(show_id, email UNIQUE)` + `created_at` + `notified_at` nullable
- email lower-cased на write
- При signup'і re-subscribe скидає `notified_at=NULL`, тож той хто не
  встиг забронити після першого notify може записатись знову
- Email шлеться **після** успішного send (`NextUnnotifiedWaitlist` +
  `MarkWaitlistNotified`), а не до — SMTP fail не залишає рядок
  "notified" без листа

### organizer
- Single-row CHECK(id=1); name/bio/contact/socials для `/about`

### audit_log
- `actor_user_id` (0 для bot/web), `actor_email`, `action`, `target`,
  `details JSON`, `created_at`. Index DESC by created_at для тіктокs
  у `/admin/audit`

## Стани сутностей

### Order
```
   ┌────────┐  CreateOrder
   │        │ ──────────────► held ──► confirmed (ConfirmOrder)
   │ (none) │                  │  │
   │        │                  │  └─► expired (expires_at < now, swept)
   │        │                  └────► cancelled (admin / user)
   └────────┘
```

### Reservation
дублює стан order'а; кожен рядок має власні `confirmed_at`, `cancelled_at`.

### Seat
не має поля стану — статус виводиться запитом:
- `seat_statuses(show_id)` → `{seat_id: free|held|sold}`
- `sold` = є reservation з confirmed_at IS NOT NULL
- `held` = reservation активна (expires_at > now, не cancelled, не confirmed)
- `free` = ні те, ні інше

### Ticket
- створюється тільки на ConfirmOrder, разом з `confirmed_at`.
- одноразовий: `UseTicket` ставить `used_at`. Повторне сканування →
  ErrTicketUsed.

## Коди

8-char base32 без визуально подібних символів (`a-z2-7`, нема
`01v8`). ~40 біт ентропії — норм для коротко-живих кодів.

- **Order code**: 8 chars, чисто `abc12345`.
- **Reservation code**: для single-seat = order code. Для multi = `<order>.N` (`abc12345.1`, `abc12345.2`).

Webhook matchить **bare 8 chars** з коментаря платежу → `FindOrderByCode`.

## Міграції

Стиль:
- Свіжа БД: великий `CREATE TABLE` блок з усіма колонками.
- Існуюча БД: послідовність `ALTER TABLE ADD COLUMN`, що SQLite тихо
  ігнорує при дублюванні.
- Backfill: `UPDATE ... WHERE col IS NULL` — idempotent.
- INSERT OR IGNORE для backfill, де може бути конфлікт UNIQUE.

Список у `internal/store/store.go::migrations`. Деталі → [[50-packages/store]].

## Codes vs Tickets

- **Code** — payload у коментарі платежу. Покупець бачить.
- **QR payload** — те, що зашите в quad-кодах. HMAC-підписане, прив'язане
  до `reservation_id` і `seat_id`. Покупець НЕ бачить (тільки сканер).

Розрізняти важливо: код для оплати ≠ код для входу.

## Дотичне

- [[50-packages/store]] — методи + міграційний список
- [[50-packages/token]] — як генерується code і QR payload
- [[70-decisions/multi-seat-orders]] — чому розділено order ↔ reservation
