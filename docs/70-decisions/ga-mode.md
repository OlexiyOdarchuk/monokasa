# ADR · GA (general admission) mode як virtual-seat pool

## Контекст

Не всі події мають сидячі місця. Standup, лекції, клубні концерти,
поетичні вечори — у залі є лімітована кількість входів, а конкретне
місце не закріплено за квитком. Купець очікує picker "скільки квитків",
не мапу залу.

До цього monokasa жив тільки в світі сидячих рядів. Кожна `show` мала
сітку `seats` з координатами row/col, і покупець мусив тицнути кружок
на мапі.

Питання: як додати GA не зламавши існуючий seated flow?

## Рішення

**`show.kind = 'seated' | 'ga'`** — нова колонка з default `'seated'`
для backward-compat. Плюс `show.ga_capacity` для збереження оригінального
розміру пулу.

**GA seats == virtual seats у тій самій таблиці.** При створенні
GA-show'у код генерує N seats з:
- `row = 1`
- `col = 1..N`
- `category = 'GA'`
- `price_kopecks = uniform price`
- `x, y` на одній лінії (на випадок якщо хтось відкриє seat editor)

Це означає що **вся існуюча інфраструктура (reservations, orders,
tickets, refunds, cancels, PDF, scans) працює без змін**. GA — просто
особлива форма seated з одним рядом.

## Чому virtual seats а не окрема ga_tickets таблиця

Окрема таблиця:
- + чистіше концептуально
- − дублювання логіки (hold, expire, sweep, scan, refund, audit)
- − дві кодові гілки в pay processor
- − дві гілки в /my, в admin guests, у scanner
- − migration ризик

Virtual seats:
- + zero нової логіки
- + єдина точка істини про hold/sold
- + scanner просто читає QR і чи seat sold
- + PDF render — додав одну if branch на `seat.Category == "GA"`
- − seats таблиця набирає 200 рядів на одну GA подію (нормально для
  SQLite, де таблиця на 50М рядів — це норма)
- − якщо seat editor відкривається на GA show, бачимо 200 кружків в
  ряд — UX-дрібниця, в admin сховали кнопку

Trade-off очевидний у бік virtual seats.

## API contract

**POST /api/admin/shows:**
```json
{
  "title": "Standup",
  "kind": "ga",
  "ga_capacity": 200,
  "price_kopecks": 25000
}
```
- `kind: "seated"` (default) — вимагає rows/cols
- `kind: "ga"` — вимагає ga_capacity, ігнорує rows/cols

**GET /api/public/shows:** додано `kind`, для GA шоу `seats_free` рахує
вільні віртуальні місця.

**GET /api/public/shows/{slug}:**
- Для GA `seats[]` повертається порожнім — frontend не може робити
  per-seat UI бо їх "немає" концептуально
- Додано `ga_capacity`, `ga_price_kopecks`, `ga_free`

**POST /api/public/orders:** додано `quantity` (mutually exclusive з
`seat_ids`). Backend викликає `AllocateFreeSeats(showID, N)`, який
повертає N перших вільних seats з пулу.

## Buyer UX

`/event/<slug>` для GA рендерить:
- Лічильник "вільно ще N з M"
- −/+ quantity picker (max = min(20, ga_free))
- Buyer name + email
- Кнопка "Забронювати і платити"

Без мапи, без seat coloring, без category легенди. attendee_names
заборонено (GA квитки взаємозамінні — підпис непомітний на PDF, бо
рендериться "Квиток №N" без row/col).

## PDF render

ticket.Seat додано Category. RenderPDF branches:
- `Category == "GA"`: header "ВХІД", великий callout "GA · квиток №N"
- Інакше: header "МІСЦЕ", "ряд X · місце Y"

QR не міняється — payload той самий формат, scanner не знає різниці.

## Trade-offs

| Проблема | Mitigation |
|---|---|
| Race: 2 buyers просять qty=100 коли вільно 150 | AllocateFreeSeats best-effort повертає що знайде; реальний атомік відбувається в CreateOrder, який поверне ErrSeatTaken якщо хтось випередив. Frontend показує "замало вільних квитків". |
| Layout editor показує GA seats у ряд | Admin не бачить link "Редактор залу" на GA show page — сховано. Прямий URL працює, але це pro-mode escape hatch. |
| Колір seat editor для GA непослідовний | Категорії заборонено для GA в admin UI (uniform price = одна категорія). |
| `seats[].col` = ticket number протекає в API | Свідомо — frontend може показати "ваш номер квитка" якщо схоче. |

## Альтернативи що НЕ обрали

- **Окрема ga_tickets таблиця**: 4x більше коду, 2 гілки в pay/scan/refund.
- **kind на reservation, не show**: ламає invariant що one event = one
  ticket type.
- **Дозволити switch seated→ga після створення**: відсутні seats треба
  генерувати, існуючі reservations прив'язані до seat_id → migration
  pain. Простіше: kind immutable, переробляєш = архівуєш + створюєш.

## Реалізація

- Schema: `shows.kind`, `shows.ga_capacity` (idempotent ALTER)
- Store: `Show.IsGA()`, `AllocateFreeSeats(showID, n)`
- Admin: створення приймає `kind`/`ga_capacity`; UI має radio
- Public: orders endpoint приймає `quantity`; getShow повертає GA fields
- Ticket: `Seat.Category` → branch render
- Frontend admin: radio в /admin/shows/new, hidden seat editor, hidden
  categories section
- Frontend buyer: quantity picker на /event/<slug>, "квиток"/"квитків"
  замість "місце"/"місць"

## Дотичне

- [[20-data-model]] — schema добавок (kind, ga_capacity, virtual seats)
- [[30-endpoints]] — quantity field у POST /api/public/orders
- [[40-flows/buyer-web]] — quantity flow
