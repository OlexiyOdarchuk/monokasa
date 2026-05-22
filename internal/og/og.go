// Package og renders Open Graph / Twitter Card meta tags into the
// embedded SPA index.html so social-bot scrapers (Telegram, Facebook,
// Twitter, Slack, Discord) get a rich link preview when somebody pastes
// an event URL.
//
// The SPA itself is static — adapter-static gives every route the same
// HTML shell. So instead of route-based <svelte:head> (which only
// updates client-side after JS runs) we inject the meta tags
// server-side per-request from this package. Bots that don't run JS
// (which is most of them) still see the right title/description/image.
package og

import (
	"bytes"
	"fmt"
	"html"
	"strings"
	"time"
)

// Props is the data needed to render OG tags for one event page.
// All fields are strings ready to be HTML-escaped — the renderer does
// the escaping, callers pass raw user input.
type Props struct {
	URL         string    // absolute https://host/event/slug
	Title       string    // show.Title
	Description string    // free-form; falls back to "<Title> · <Venue> · <Date>"
	ImageURL    string    // absolute https://… poster URL; empty = skip image tags
	SiteName    string    // "monokasa" by default
	StartsAt    time.Time // appended to fallback description; zero skips it
	Venue       string    // appended to fallback description; empty skips
}

// Render returns a copy of indexHTML with OG/Twitter meta tags injected
// immediately before </head>. The page's original <title> is also
// rewritten so social previews and browser tabs agree.
func Render(indexHTML []byte, p Props) []byte {
	tags := buildTags(p)
	title := strings.TrimSpace(p.Title)
	if title == "" {
		return indexHTML
	}

	// Rewrite the existing <title>…</title> in place so the tab title
	// matches what the bot scraper sees. SPA can still override it
	// client-side via <svelte:head>, but we want both correct from the
	// first paint for social previews.
	out := replaceTitle(indexHTML, title)

	// Inject the meta block just before </head>. Case-insensitive
	// search keeps us safe against future toolchain quirks.
	idx := bytes.Index(bytes.ToLower(out), []byte("</head>"))
	if idx < 0 {
		// Malformed HTML — bail by appending at the top of <body>, or
		// return as-is if we can't find <body> either. Bots will then
		// fall back to whatever was in the original head.
		return out
	}
	var buf bytes.Buffer
	buf.Grow(len(out) + len(tags) + 16)
	buf.Write(out[:idx])
	buf.WriteString("\n\t\t")
	buf.WriteString(tags)
	buf.WriteString("\n\t")
	buf.Write(out[idx:])
	return buf.Bytes()
}

// buildTags assembles the meta-tag block. Each line is HTML-escaped at
// the boundary so an event title containing `"` or `<` can't break out
// of the attribute.
func buildTags(p Props) string {
	desc := strings.TrimSpace(p.Description)
	if desc == "" {
		desc = fallbackDescription(p)
	}
	site := strings.TrimSpace(p.SiteName)
	if site == "" {
		site = "monokasa"
	}

	var b strings.Builder
	// Plain meta + canonical first — search engines and link previews
	// both honour them.
	if d := desc; d != "" {
		b.WriteString(meta("description", d))
	}
	if p.URL != "" {
		fmt.Fprintf(&b, `<link rel="canonical" href=%q>`+"\n", html.EscapeString(p.URL))
	}
	// Open Graph (Facebook, Telegram, LinkedIn, Slack, Discord).
	b.WriteString(metaProp("og:type", "website"))
	b.WriteString(metaProp("og:site_name", site))
	b.WriteString(metaProp("og:title", p.Title))
	if desc != "" {
		b.WriteString(metaProp("og:description", desc))
	}
	if p.URL != "" {
		b.WriteString(metaProp("og:url", p.URL))
	}
	if p.ImageURL != "" {
		b.WriteString(metaProp("og:image", p.ImageURL))
	}
	// Twitter Card (also picked up by some bots that ignore OG).
	cardKind := "summary"
	if p.ImageURL != "" {
		cardKind = "summary_large_image"
	}
	b.WriteString(meta("twitter:card", cardKind))
	b.WriteString(meta("twitter:title", p.Title))
	if desc != "" {
		b.WriteString(meta("twitter:description", desc))
	}
	if p.ImageURL != "" {
		b.WriteString(meta("twitter:image", p.ImageURL))
	}
	return b.String()
}

func meta(name, content string) string {
	return fmt.Sprintf(`<meta name=%q content=%q>`+"\n\t\t",
		html.EscapeString(name), html.EscapeString(content))
}

func metaProp(prop, content string) string {
	return fmt.Sprintf(`<meta property=%q content=%q>`+"\n\t\t",
		html.EscapeString(prop), html.EscapeString(content))
}

// replaceTitle swaps the contents of the first <title>…</title> block.
// Used to make the tab title match the OG title without depending on JS.
func replaceTitle(in []byte, newTitle string) []byte {
	lower := bytes.ToLower(in)
	start := bytes.Index(lower, []byte("<title>"))
	if start < 0 {
		return in
	}
	end := bytes.Index(lower[start:], []byte("</title>"))
	if end < 0 {
		return in
	}
	end += start
	openLen := len("<title>")
	var buf bytes.Buffer
	buf.Grow(len(in) + len(newTitle))
	buf.Write(in[:start+openLen])
	buf.WriteString(html.EscapeString(newTitle))
	buf.Write(in[end:])
	return buf.Bytes()
}

// fallbackDescription synthesises a description when none is provided —
// "Концерт · Atlas · 7 червня 2026, 19:00", roughly. Skips empty parts.
func fallbackDescription(p Props) string {
	parts := []string{}
	if v := strings.TrimSpace(p.Venue); v != "" {
		parts = append(parts, v)
	}
	if !p.StartsAt.IsZero() {
		parts = append(parts, formatDate(p.StartsAt))
	}
	if len(parts) == 0 {
		return "Купити квиток через monobank"
	}
	return strings.Join(parts, " · ")
}

var ukMonths = [...]string{
	"січня", "лютого", "березня", "квітня", "травня", "червня",
	"липня", "серпня", "вересня", "жовтня", "листопада", "грудня",
}

func formatDate(t time.Time) string {
	return fmt.Sprintf("%d %s %d, %02d:%02d",
		t.Day(), ukMonths[int(t.Month())-1], t.Year(), t.Hour(), t.Minute())
}

// AbsoluteImageURL turns a possibly-relative poster URL into one bots
// can fetch. baseURL is the canonical site origin ("https://kasa.ex.com");
// relativePosterURL is whatever the show stored (e.g. "/posters/abc.jpg"
// from the upload endpoint, or a full external https://...). Returns
// "" when both are blank so the caller can skip image tags.
func AbsoluteImageURL(baseURL, posterURL string) string {
	p := strings.TrimSpace(posterURL)
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return p
	}
	b := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if b == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return b + p
}
