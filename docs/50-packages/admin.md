# 50 · Package: admin

Authenticated API для адмін-вебу. Шлях `/api/admin/*`, за middleware
`RequireAuth` (cookie-сесія, див. [[50-packages/auth]]).

## Endpoints

Повний реєстр → [[30-endpoints]].

Скорочено:
- `GET /me` — поточний адмін
- `GET/POST /shows`, `GET/PATCH /shows/{id}`, `POST /shows/{id}/archive`
- `GET /shows/{id}/seats`, `POST /shows/{id}/seats`, `PATCH /seats` (batch), `DELETE /seats/{id}`
- `GET /shows/{id}/guests`, `GET /shows/{id}/guests.csv`
- `POST /reservations/{id}/cancel` — cascade на весь order + сповіщення
- `POST /posters` — multipart upload (делегує `posters.HandleUpload`)

## Cancel notifier hook

```go
adminH.SetCancelNotifier(func(ctx, res, seat) {
    if res.TGChatID != 0 {
        go bot.SendCancellation(res.TGChatID, seat)
    }
    if res.BuyerEmail != "" && email != nil {
        go email.SendCancellationEmail(ctx, res.BuyerEmail, res.BuyerName, seat, show)
    }
})
```

Виклик асинхронний (`go`), помилки доставки тільки логуються — БД вже
консистентна.

## Batch seat update

`PATCH /api/admin/seats` приймає `{seats: [{id, x?, y?, label?, category?, price_kopecks?, sellable?}]}`. Усі pointer-поля nullable → null = "не міняти". Виконується в одному tx.

Потрібно для drag&drop редактора — він збирає купу змін у `SvelteSet`
"dirty" і шле одним PATCH.

## Залежності

```go
type Handler struct {
    st              *store.Store
    cancelNotifier  func(context.Context, store.Reservation, store.Seat)
}
```

## Файли

- `admin.go` — handlers + response types
- `admin_test.go` — httptest з fake session

## Дотичне

- [[40-flows/admin]] — як це використовується UI
- [[50-packages/auth]] — як проходить RequireAuth
- [[30-endpoints]] — повний реєстр
