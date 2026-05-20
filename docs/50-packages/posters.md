# 50 · Package: posters

Multipart upload афіш + static serve. Дві ручки, простий пакет.

## Public API

```go
type Service struct { dir string }

func New(dir string) (*Service, error)            // mkdir -p; перевірка прав
func (s *Service) HandleUpload(w, r)              // POST /api/admin/posters
func (s *Service) HandleServe(w, r)               // GET  /posters/<filename>
```

## Upload — `HandleUpload`

1. `http.MaxBytesReader(r.Body, 5*1024*1024)` — soft cap 5 МБ.
2. `r.ParseMultipartForm(5MB)`.
3. `FormFile("file")` — читаємо повністю в `[]byte`.
4. `http.DetectContentType(buf)` — приймаємо тільки `image/jpeg`,
   `image/png`, `image/webp`. Інше → 415 `unsupported_type`.
5. `crypto/rand` 16 байт → `hex` 32 chars → filename `<hex>.<ext>`.
6. `os.WriteFile(filepath.Join(dir, filename), buf, 0644)`.
7. Відповідь: `{ "url": "/posters/<filename>" }`.

Адмін копіює цей URL у `poster_url` поля show через окремий PATCH.

## Serve — `HandleServe`

1. Витягуємо `filename` з `r.URL.Path` (після `/posters/`).
2. Перевірка path traversal: `filepath.Clean(filename)` → має дорівнювати
   filename; інакше 400.
3. `os.Open(filepath.Join(dir, filename))` → `io.Copy` у response з
   `Content-Type` за розширенням.
4. `Cache-Control: public, max-age=31536000, immutable` (filename
   містить хеш, тому міняється з контентом).

## Конфіг

- `POSTERS_DIR` ENV. Default локально — `posters` (relative); Docker
  compose explicitly ставить `/data/posters`.
- Дир створюється `os.MkdirAll(dir, 0755)` при `New(...)`. Якщо нема
  прав — fatal error при старті.

## Файли

- `posters.go` — Service, обидва handler'и
- (тестів поки нема — handlers прості, перевіряються integration'ом)

## Дотичне

- [[40-flows/admin]] — UI flow завантаження
- [[60-deploy]] — про шлях у Docker
