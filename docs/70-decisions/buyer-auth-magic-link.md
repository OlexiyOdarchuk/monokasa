# ADR · Чому buyer auth — magic-link, не email+password

## Контекст

Покупцям потрібно бачити свої квитки на сайті — переглядати QR без
копання в листах, перевіряти статус після оплати, втратити PDF і
відновити. Це вимагає якоїсь форми аутентифікації.

Варіанти, які ми розглядали:

1. **Email + password** — класична реєстрація з паролем
2. **Magic-link** — passwordless, email отримує одноразове посилання
3. **Permalink у листі** — без жодної аутентифікації, кожен лист містить
   токен на сторінку квитка
4. **OAuth** — Google / Apple sign-in

## Рішення

**Magic-link** з 15-хвилинним одноразовим токеном і 30-денним cookie
session.

## Чому НЕ password

- **Більше коду**: треба bcrypt-хешування, password validation,
  forgot-password flow з email-reset (по суті — той же magic-link, але
  на додачу до основного потоку), rate-limiting на login attempts.
- **Гірший UX**: користувач пам'ятає пароль раз на півроку для одного
  купленого квитка. Половина забуде, піде через "forgot password" →
  отримає email → magic-link де-факто.
- **Не безпечніше**: контроль над email = контроль над акаунтом (через
  reset). Magic-link не додає вразливості, лише прибирає крок.

## Чому НЕ permalink у листі

- **Не масштабується**: 5 покупок = 5 permalink'ів у різних листах.
  Користувач не пам'ятає де який. "Покажи свої квитки на майбутнє" —
  розгубленість.
- **Email = source of truth**: якщо втратив листи (видалив, змінив
  адресу) — квитки втрачені. Magic-link дає універсальний вхід поки є
  доступ до email.
- **Складніше скасувати/змінити**: permalink живе вічно або має
  складний lifecycle. Cookie session має тривіальний logout.

## Чому НЕ OAuth

- **Залежність від third-party**: Google/Apple можуть змінити вимоги,
  заблокувати додаток без причини.
- **Більше DNS/secrets налаштування**: OAuth credentials, redirect
  URIs, app verification.
- **Не вирішує проблему**: покупець міг купити квитки на гостьовий
  email, який не прив'язаний до Google акаунта.

## Trade-offs обраного рішення

| Trade-off | Mitigation |
|---|---|
| Залежність від SMTP | Без SMTP login просто не запрацює (так само як без monobank-jar платежі); dev-fallback логує лінк у консоль для локального тесту |
| Email може лагати/спам | Token живе 15 хв; якщо запізнився — запроси ще раз |
| Втрата доступу до email = втрата акаунта | Так само як з reset-password flow; організатор може вручну надіслати посилання покупцеві |
| Сесія 30 днів — компроміс зі security | Можна `/api/public/login/logout` явно; HttpOnly + SameSite=Lax мінімізує XSS/CSRF |

## Реалізація

Деталі flow → [[40-flows/buyer-my-tickets]].

Ключові аспекти:
- **Token**: 256-bit, hex, crypto/rand-mint
- **One-shot**: ConsumeBuyerLoginToken under transaction із `used_at` UPDATE
- **TTL**: 15 хв на login token, 30 днів на cookie session
- **Magic-link points at consume endpoint directly** (а не /my?token=)
  — браузер обробляє 303 + Set-Cookie нативно. Fetch-based підхід ламав
  cookie в деяких браузерах (opaqueredirect).

## Майбутні розширення

- TG-link login: якщо buyer відомий через bot deep-link, можна
  залогінити через bot ("/start login_xxx" → бот ставить cookie).
- Магічний "не маю доступу до email" workflow — admin вручну видає
  permalink через адмінку.
- 2FA для топ-events: TOTP після першого magic-link login. Поки не
  потрібно.

## Дотичне

- [[40-flows/buyer-my-tickets]] — повний flow
- [[50-packages/email]] — як шле лист
