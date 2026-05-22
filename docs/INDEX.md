# monokasa · мозок проєкту

Vault з усім, що варто знати про monokasa: ідея, архітектура, як працює
кожен flow, що в кожному пакеті, як це деплоїти, чому такі рішення.
README.md — для тих, хто хоче ЗАПУСТИТИ. Цей vault — для тих, хто хоче
**розуміти і змінювати**.

Формат: Obsidian-style markdown з `[[wikilinks]]`. Файли пронумеровані,
щоб у Files-сайдбарі вони стояли в логічному порядку читання.

## Швидка навігація

### 🎯 Що це взагалі
- [[00-idea]] — vision, scope, non-goals; що ми НЕ робимо і чому

### 🏗 Як це збудовано
- [[10-architecture]] — пакети, dependency direction, чому монолі т
- [[20-data-model]] — схема БД, життєвий цикл сутностей, коди
- [[30-endpoints]] — карта всіх HTTP-route'ів + бот-команд

### 🌊 Сценарії
- [[40-flows/buyer-web]] — покупець через сайт
- [[40-flows/buyer-bot]] — покупець через бот
- [[40-flows/buyer-my-tickets]] — "Мої квитки" magic-link flow
- [[40-flows/admin]] — операції адміна
- [[40-flows/webhook]] — monobank → confirm → доставка
- [[40-flows/reconcile]] — рятувальна сітка для пропущених webhook'ів

### 📦 Що в кожному пакеті
- [[50-packages/store]] — SQLite, схема, міграції, CRUD
- [[50-packages/pay]] — обробник платежів
- [[50-packages/bot]] — Telegram
- [[50-packages/public]] — публічний API
- [[50-packages/admin]] — адмін-API
- [[50-packages/auth]] — bcrypt-сесії
- [[50-packages/email]] — SMTP-доставка
- [[50-packages/posters]] — upload афіш
- [[50-packages/realtime]] — SSE hub для live seat updates
- [[50-packages/ticket]] — рендер PDF
- [[50-packages/token]] — коди + HMAC для QR
- [[50-packages/web]] — сканер на вході
- [[50-packages/webui]] — embed для Svelte SPA

### 🚢 Деплой
- [[60-deploy]] — Docker, cloudflared, env, troubleshooting

### 📐 Рішення (ADR)
- [[70-decisions/why-self-host]] — чому НЕ SaaS
- [[70-decisions/why-monobank-jar]] — чому банка моно, не еквайринг
- [[70-decisions/why-sqlite]] — чому SQLite, не Postgres
- [[70-decisions/multi-seat-orders]] — чому orders + reservations, а не плоско
- [[70-decisions/attendee-names]] — як обрали B (один + опційно)
- [[70-decisions/buyer-auth-magic-link]] — чому magic-link, не password

### 📖 Терміни
- [[80-glossary]] — order, reservation, hold, jar, attendee, тощо

## Як читати залежно від ролі

- **Хочу зрозуміти проєкт з нуля** → [[00-idea]] → [[10-architecture]] → [[40-flows/buyer-web]]
- **Збираюсь додавати фічу** → [[10-architecture]] → відповідний `50-packages/<pkg>` → [[20-data-model]]
- **Налаштовую прод** → README → [[60-deploy]]
- **Дебажу баг** → [[40-flows]] на той сценарій, потім пакет, у якому
  баг
- **Хочу зрозуміти "чому так"** → [[70-decisions]]
