// Package webui serves the Svelte SPA embedded into the Go binary.
//
// In production the Docker multi-stage build runs `vite build` and copies
// the result into ./dist before `go build`, so the entire frontend ships
// inside the single binary — no Node runtime, no static-file server, one
// container, one port.
//
// In local `go build` (without the Docker pipeline) ./dist contains only
// a stub index.html pointing the developer at the right rebuild command.
// embed requires the directory to exist with at least one file, hence the
// stub.
package webui

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// Handler serves the embedded SPA. GET to a path that exists in the bundle
// returns the file (with the right content type and caching). Anything
// else falls back to index.html so the SvelteKit client-side router can
// take over — that's what `adapter-static` with `fallback: 'index.html'`
// expects from the host.
type Handler struct {
	files     http.FileSystem
	indexHTML []byte
}

func New() (*Handler, error) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil, fmt.Errorf("sub fs: %w", err)
	}
	idx, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return nil, fmt.Errorf("read index.html (run `npm run build` in frontend/ or rebuild via docker): %w", err)
	}
	return &Handler{files: http.FS(sub), indexHTML: idx}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		h.writeIndex(w)
		return
	}
	f, err := h.files.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			h.writeIndex(w)
			return
		}
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || stat.IsDir() {
		h.writeIndex(w)
		return
	}
	// Long-cache hashed assets under /_app/ (SvelteKit's immutable bundle
	// dir) — names contain a content hash, so they're safe to cache forever.
	// Everything else gets a short cache to stay responsive to redeploys.
	if strings.HasPrefix(path, "_app/immutable/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=300")
	}
	http.ServeContent(w, r, stat.Name(), stat.ModTime(), f)
}

func (h *Handler) writeIndex(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(h.indexHTML)
}
