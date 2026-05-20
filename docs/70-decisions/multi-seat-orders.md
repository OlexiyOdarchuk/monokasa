# ADR · Чому orders + reservations, а не плоско

## Контекст

Спершу була одна таблиця `reservations` — кожен рядок = одне місце +
одна оплата. Покупець міг придбати лише 1 місце за раз. Запит від
користувача: "хочу 4 квитки одним платежем".

## Рішення

**Додали окрему таблицю `orders`. Один order групує N reservations
під одним 8-char base32 кодом, що йде у коментар платежу.**

Кожна reservation отримує сабкод: для single-seat — той самий `code`,
для multi — `code.1`, `code.2`, … (бо `reservations.code` UNIQUE).

## Чому НЕ одна таблиця

### Альтернатива A: групувати reservations за код

```sql
SELECT * FROM reservations WHERE code = 'abc12345'
```
- Поламав би UNIQUE constraint на `reservations.code` (потрібен для
  legacy code; без нього webhook стало б важче дебажити).
- Buyer fields (name, email, TGChatID) дублювалися б у N рядках.
- Зміна email на ордері = N UPDATE'ів замість одного.
- Скасування "всього замовлення" = batch UPDATE.

### Альтернатива B: змінити `reservations.code` на не-унікальний

- Webhook матчить **код**. Якщо code не унікальний, треба ще щось у
  comment'і моно — а покупець не зможе вписати UUID.

### Обраний варіант: окрема `orders`

- `orders.code` UNIQUE, 8-char — це **публічний** код для платежу.
- `reservations.code` теж UNIQUE, але як `<order>.<seq>` — це
  внутрішня деталь для legacy queries (FindReservationByCode тощо).
- Buyer info живе на `orders`. `reservations` дублює `buyer_name`,
  `buyer_email` тільки для денормалізованих queries (наприклад,
  guests list по show). Це OK trade-off.
- Cancel cascade — один SQL на order, чисто.

## Single-seat backward compat

Старі рядки `reservations` отримали бекфіл-міграцію:
```sql
INSERT OR IGNORE INTO orders (...) SELECT ... FROM reservations WHERE order_id IS NULL;
UPDATE reservations SET order_id = (SELECT id FROM orders WHERE code = reservations.code) WHERE order_id IS NULL;
```

Кожен старий single-seat reservation → 1-row order. Жодного спеціального
коду для "legacy reservations без order_id".

Метод `Reserve` (single-seat shortcut) — це shim над
`CreateOrder([]Seat{seat})`:
```go
_, reservations, _ := CreateOrder(ctx, []Seat{seat}, ..., nil, code, hold)
return reservations[0], nil
```

## Сабкоди — чому така схема

Альтернативи розглянуто:
- `code` без сабкоду + не-унікальний → проблеми вище.
- `<code>-<n>` із dash — теж OK, але dot не зустрічається у base32
  alphabet, легше парсити.
- `<n>:<code>` — менш зчитувано в логах.

Дотик: `FindReservationByCode("abc12345")` для single-seat працює
"з коробки" (бо там code = `abc12345`). Для multi-seat — треба знати
сабкод `abc12345.1`. Це OK, бо multi-seat reservation lookup йде через
`FindOrderByCode`, не reservation.

## Attendee names — наслідок

Багато-seat order відкрив питання "чиє ім'я на квитку". → [[attendee-names]].

## Дотичне

- [[20-data-model]] — повна схема
- [[50-packages/store]] — `CreateOrder`, `FindOrderByCode`, `ConfirmOrder`
- [[40-flows/buyer-web]] — як використовується frontend'ом
