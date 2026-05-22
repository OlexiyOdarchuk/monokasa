# ADR · Категорії місць замість per-seat price-string-mapping

## Контекст

Buyers очікують concert.ua-style мапу залу з кольоровими зонами:
VIP — червоне, Standard — синє, Pit — зелене, кожна зона має свою
ціну. До цього monokasa мав:

- `seats.category` — довільний string-label на місце ("vip", "balcony")
- `seats.price_kopecks` — per-seat ціна
- Колір — хешований з category string на frontend (стабільний, але
  не керований)

Проблеми:
- Admin не міг змінити ціну зони одним рухом (треба було колупати
  per-seat patches у seat editor).
- Кольори не пов'язані з price tier — buyer бачив "зелене" місце за
  500₴ і "синє" за 250₴ без логіки.
- Не було легенди — "що означає синій?".

## Рішення

**Окрема таблиця `seat_categories(show_id, name, color, price_kopecks)`.**

- Admin створює "tier" з назвою, кольором і ціною
- `seats.category` (string) joins до `seat_categories.name` у тому ж show
- При upsert категорії — batch UPDATE на всі seats з matching name,
  їхня price_kopecks стає = category.price_kopecks
- Frontend читає legend з `categories[]` у `/api/public/shows/{slug}`
- Колір місця — лookup за `category` name; fallback на старий
  hashed-hue алгоритм для unknown labels

## Що НЕ міняли

- `seats.category` залишається довільним string'ом. Не FK. Не
  обов'язковим.
- `seats.price_kopecks` залишається SOURCE OF TRUTH для пейменту —
  category just maintains it synced via UPDATE.
- Старі shows без категорій працюють як раніше (default colour).

## Trade-offs

| Проблема | Mitigation |
|---|---|
| Адмін перейменує category в editor — старі seats з `category="VIP"` стають orphan label | Свідомо: ми не блюмо seats.category на delete. Admin може просто upsert назад або bulk-edit у seat editor. |
| Race: 2 admins створюють однойменну категорію | UNIQUE(show_id, name) constraint + INSERT ... ON CONFLICT UPDATE — детермінований останній-виграє. |
| Category price out of sync з seat price після ручної правки місця | Admin може ручно поставити exception (одне місце з нестандартною ціною). Upsert category → це reset. |

## Альтернативи що НЕ обрали

- **Робити seats.category foreign key → seat_categories.id**: ламає
  легкий migration з existing shows, потребує backfill.
- **Зробити price тільки у category, прибрати seat.price_kopecks**:
  ламає всі історичні seats без категорії; pay processor би міг
  ламнутись.

## Реалізація

- Schema: `seat_categories` table, 1:N від shows
- Store: `ListSeatCategories`, `UpsertSeatCategory` (з cascade-update),
  `DeleteSeatCategory` (не торкає seats)
- Admin endpoints: GET/POST under `/api/admin/shows/{id}/categories`,
  DELETE `/api/admin/categories/{id}`
- Public `/api/public/shows/{slug}` тепер повертає `categories[]`
- Frontend: section "Категорії місць" на admin show page; легенда з
  кольорами + цінами на buyer event page

## Дотичне

- [[20-data-model]] — schema добавок
- [[50-packages/admin]] — endpoint'и
