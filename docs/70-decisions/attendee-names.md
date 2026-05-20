# ADR · Per-ticket attendee names

## Контекст

Після додавання multi-seat orders ([[multi-seat-orders]]) виникло
питання: якщо я купую 4 квитки для друзів, чиї імена будуть на PDF?

Користувач спитав: "чому одне ім'я на всіх 4-х квитках?".

## Варіанти

### A. Питати N імен одразу

Форма має N полів `Ім'я на квиток 1`, `Ім'я на квиток 2`, …

- ➕ кожен квиток персоналізований, охорона на дверях знає кого пускати
- ➖ вписувати 6 імен на телефоні — пекло
- ➖ навіть якщо одне ім'я на всіх — все одно треба заповнити N разів

### B. Одне ім'я → "Замовник" + опційні per-ticket attendee

Дефолт: ім'я замовника на всіх PDF. Якщо хоче — toggle і per-seat
поля з placeholder'ом ім'я замовника.

- ➕ дефолт = "купую для своєї компанії" — мінімум фрикції
- ➕ персоналізація можлива, якщо треба
- ➕ це шаблон, що використовує більшість майданчиків (Ticketmaster,
  Concert.ua)
- ➖ ще одне поле в DB
- ➖ ще одна гілка в pay-renderer'і

### C. Тільки одне ім'я

Як зараз. Залишити так і не ускладнювати.

- ➕ простіше
- ➖ не вирішує запит користувача

## Рішення

**B. Один + опційні per-ticket attendee names.**

Вибране, бо:
- Users-перший флоу не змінюється (тапнув "Забронювати" — одне поле
  ім'я, як раніше).
- Хто хоче — клацає `✏️ Підписати квитки на різні імена`, побачив
  inline-поля на кожне місце.
- На бекенді обробка тривіальна: nullable string per row, fallback
  на buyerName у рендерері.

## Реалізація

### Schema

```sql
ALTER TABLE reservations ADD COLUMN attendee_name TEXT NOT NULL DEFAULT '';
```

`''` = "fall back to BuyerName at render time".

### Store

`CreateOrder(ctx, seats, ..., attendeeNames []string, code, hold)` —
`attendeeNames` опційний (nil або len(seats)).

### Pay processor

```go
name := it.AttendeeName
if name == "" {
    name = order.BuyerName
}
pdf, _ := Renderer(show, it.Seat, name, qr)
```

### Public API

`POST /api/public/orders` приймає опційний `attendee_names: [string]`.
Якщо є — довжина має співпадати з seat_ids. Кожен рядок проходить
`normalizeName` (2..60 рун). Empty entries → "".

### Frontend

Toggle `✏️ Підписати квитки на різні імена` (показується при >=2
обраних). Inline-input з placeholder = ім'я замовника. Відправляється
тільки якщо хоч одне поле заповнене (інакше field відсутній у JSON,
бекенд бачить як "не задано").

### Bot

**Per-attendee не питаємо.** Усі multi-seat бот-замовлення йдуть з
одним ім'ям замовника. Свідомо: типувати N імен на телефоні — погано;
основна цінність бот-flow — швидкість.

## Дотичне

- [[multi-seat-orders]] — батьківське рішення
- [[50-packages/pay]] — де fallback живе
- [[50-packages/public]] — валідація
