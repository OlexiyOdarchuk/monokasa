# 80 · Glossary

Терміни і скорочення, що зустрічаються в коді / комітах / документації.

## Доменні

**Show** — подія. Має title, venue, starts_at, зал N×M, опційно постер
і опис. `ArchivedAt != nil` ховає з landing і боту.

**Seat** — місце в залі. Унікальне за `(show_id, row, col)`. Має `x, y`
для drag&drop, опційно `label`, `category` (для кольору). `sellable=false`
— проходи / технічні слоти, не продаються.

**Order** — група reservations під одним 8-char base32 кодом. Один
order = один платіж. Buyer info живе тут.

**Reservation** — hold на одне seat у складі order. Має сабкод (`code`
або `code.N`). Стан = combo `confirmed_at`/`cancelled_at`/`expires_at`.

**Ticket** — PDF із QR. Створюється на confirm; `used_at` ставиться
сканером.

**Code** — публічний 8-char base32 (без `loi01v8`) ідентифікатор
order'а. Йде в коментар платежу моно.

**QR payload** — HMAC-підписаний base64-рядок зашитий у QR. Перевіряється
сканером. ≠ Code.

**Hold** — стан reservation, де `expires_at > now` і `confirmed_at IS
NULL`. За дефолтом 15 хв. Sweep гасить минулі.

**Attendee** — опційне ім'я на конкретний квиток. Empty → fallback на
BuyerName замовника.

**Jar** — банка моно (https://send.monobank.ua/jar/...). Платіжний
канал у monokasa.

**Reconcile** — догрібати пропущені webhook'и через monobank
statement. Запускається `/reconcile` від адмін-бота.

## Технічні

**RequireAuth** — middleware у `internal/auth`. Перевіряє cookie-сесію.
303 для browser-Accept, 401 для API.

**SeatStatus** — derived enum (`free | held | sold`), не таблична
колонка. Виводиться запитом `SeatStatuses(showID)`.

**ShowFn** — closure, що повертає поточну "активну подію". Дозволяє
адміну редагувати без рестарту бота / pay.

**SeatPatch** — partial-update для seat. nullable pointer fields → null
= "не міняти".

**SvelteSet** — фронтенд: `import { SvelteSet } from 'svelte/reactivity'`.
Реактивний Set для basket-state. `$state(new Set())` не завжди трігерить
re-render у Svelte 5, тому SvelteSet.

## Сценарні

**Bootstrap admin** — створення першого адмін-юзера через `ADMIN_EMAIL`
+ `ADMIN_PASSWORD` ENV при першому старті. Далі ці ENV ігноруються.

**Deep link** — Telegram URL `t.me/<bot>?start=res_<code>`. Прив'язує
TG-чат до order'а, щоб після оплати PDF прийшов і у Telegram.

**Force-cancel** — `AdminCancelReservation` зі сповіщенням buyer'у.
Cascade на весь order (всі rows + order row).

**Sweep** — періодичний background-таск:
- `SweepExpiredHolds` — cancel-ить order'и з минулим `expires_at`
- `SweepSessions` — видаляє минулі admin sessions

**Reminder loop** — таск, що шле "сьогодні о 19:00" повідомлення за
годину до старту події. Маркує `reminded_at` щоб не дублювати.

## Скорочення в логах

- `tgUserId`, `tgChatId` — Telegram identifiers
- `showId`, `seatId`, `reservationId`, `orderId` — БД primary keys
- `code` — order code (8-char)
- `slug` — show URL handle

## Дотичне

- [[20-data-model]] — як ці сутності лежать у БД
- [[40-flows/buyer-web]] — як вони рухаються через flow
