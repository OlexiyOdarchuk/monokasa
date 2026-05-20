// Package posters stores admin-uploaded show posters on disk and serves
// them under /posters/<filename> without auth. Files live in a directory
// the operator points at via POSTERS_DIR (default /data/posters in the
// docker image; the same volume that holds SQLite).
//
// Storage is intentionally trivial — content-addressed-ish (random
// 16-byte filename) with the original content-type's extension. No
// resizing, no EXIF stripping, no S3. A 5MB-per-file cap and an
// extension whitelist are the only guards.
package posters

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Service handles uploads and serving. Construct with New; both HTTP
// handlers are exposed for the caller to mount on whichever mux.
type Service struct {
	dir string
}

// New ensures the storage directory exists (mkdir -p) and returns a
// configured Service.
func New(dir string) (*Service, error) {
	if dir == "" {
		return nil, errors.New("posters: empty dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("posters: mkdir %s: %w", dir, err)
	}
	return &Service{dir: dir}, nil
}

// Dir returns the on-disk directory where files live.
func (s *Service) Dir() string { return s.dir }

// HandleUpload accepts multipart/form-data with a "file" field. Validates
// size and content type, writes the bytes to disk under a random name,
// returns the public URL.
func (s *Service) HandleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Cap the whole request at 6 MiB — 5 MiB file + form overhead. Anything
	// larger gets cut by MaxBytesReader before we burn memory on it.
	r.Body = http.MaxBytesReader(w, r.Body, 6<<20)
	if err := r.ParseMultipartForm(6 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "too_large", "файл завеликий (максимум 5 MB)")
		return
	}
	defer r.MultipartForm.RemoveAll() // delete tmp files mux created

	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "no_file", "очікую multipart field 'file'")
		return
	}
	defer f.Close()
	if hdr.Size > 5<<20 {
		writeError(w, http.StatusBadRequest, "too_large", "файл більше 5 MB")
		return
	}

	// Sniff content type from the first 512 bytes — don't trust the
	// client-supplied Content-Type, browsers attach whatever they want.
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	ct := http.DetectContentType(head)
	ext, ok := extForContentType(ct)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_format",
			"тільки JPEG / PNG / WebP / GIF (отримано "+ct+")")
		return
	}

	// Reset reader to the start so we can re-stream the full body to disk.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}

	name, err := randName()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	fname := name + ext
	dst, err := os.Create(filepath.Join(s.dir, fname))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal",
			"не вдалось зберегти файл")
		return
	}
	defer dst.Close()
	if _, err := io.Copy(dst, f); err != nil {
		_ = os.Remove(dst.Name())
		writeError(w, http.StatusInternalServerError, "write_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{
		"url": "/posters/" + fname,
	})
}

// HandleServe is a wrapped http.FileServer that refuses directory
// listings (no index.html, just deny). Cache headers are long because
// filenames are content-random and never overwritten.
func (s *Service) HandleServe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/posters/")
	// Reject anything funny — leading dot, slash, drive letters. The
	// filenames we emit are 32 hex chars + .ext, ASCII only.
	if name == "" || strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(s.dir, name)
	// Belt-and-braces: make sure the resolved path stays inside dir.
	abs, err := filepath.Abs(path)
	if err != nil || !strings.HasPrefix(abs, filepath.Clean(s.dir)) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, path)
}

// extForContentType maps the http.DetectContentType output to a file
// extension. Browser default Content-Type's are unreliable; this list
// is small on purpose — admins paste real photos, not exotic formats.
func extForContentType(ct string) (string, bool) {
	switch {
	case strings.HasPrefix(ct, "image/jpeg"):
		return ".jpg", true
	case strings.HasPrefix(ct, "image/png"):
		return ".png", true
	case strings.HasPrefix(ct, "image/webp"):
		return ".webp", true
	case strings.HasPrefix(ct, "image/gif"):
		return ".gif", true
	}
	return "", false
}

// randName returns 32 hex chars from crypto/rand — plenty for our
// "guess-the-poster-URL" threat model (32 hex = 128 bits).
func randName() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, map[string]string{"error": code, "detail": detail})
}
