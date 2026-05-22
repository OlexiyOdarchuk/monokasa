package og

import (
	"strings"
	"testing"
	"time"
)

const stubIndex = `<!doctype html>
<html lang="uk">
	<head>
		<meta charset="utf-8" />
		<title>monokasa</title>
	</head>
	<body></body>
</html>`

func TestRenderInjectsRequiredTags(t *testing.T) {
	out := Render([]byte(stubIndex), Props{
		URL:      "https://kasa.example.com/event/standup",
		Title:    "Standup-марафон",
		Venue:    "Atlas",
		StartsAt: time.Date(2026, 6, 7, 19, 0, 0, 0, time.UTC),
		ImageURL: "https://kasa.example.com/posters/abc.jpg",
	})
	s := string(out)
	for _, want := range []string{
		`property="og:title"`,
		`content="Standup-марафон"`,
		`property="og:url"`,
		`content="https://kasa.example.com/event/standup"`,
		`property="og:image"`,
		`name="twitter:card"`,
		`content="summary_large_image"`,
		`<title>Standup-марафон</title>`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in output\n%s", want, s)
		}
	}
}

func TestRenderEscapesHTML(t *testing.T) {
	out := Render([]byte(stubIndex), Props{
		URL:   "https://kasa.example.com/event/x",
		Title: `Bad "title" <script>`,
	})
	s := string(out)
	if strings.Contains(s, "<script>") {
		t.Fatalf("unescaped <script> leaked into HTML:\n%s", s)
	}
	if !strings.Contains(s, `content="Bad &#34;title&#34; &lt;script&gt;"`) {
		t.Errorf("expected escaped title attribute, got:\n%s", s)
	}
}

func TestRenderSkipsImageTagsWhenAbsent(t *testing.T) {
	out := Render([]byte(stubIndex), Props{
		URL: "https://kasa.example.com/event/x", Title: "X",
	})
	s := string(out)
	if strings.Contains(s, "og:image") {
		t.Errorf("og:image emitted with no image URL:\n%s", s)
	}
	if !strings.Contains(s, `content="summary"`) {
		t.Errorf("expected twitter:card summary (not large image):\n%s", s)
	}
}

func TestRenderEmptyTitleNoOp(t *testing.T) {
	out := Render([]byte(stubIndex), Props{URL: "x"})
	if string(out) != stubIndex {
		t.Errorf("empty title should leave index untouched")
	}
}

func TestAbsoluteImageURL(t *testing.T) {
	cases := []struct {
		base, poster, want string
	}{
		{"https://k.example.com", "/posters/x.jpg", "https://k.example.com/posters/x.jpg"},
		{"https://k.example.com/", "/posters/x.jpg", "https://k.example.com/posters/x.jpg"},
		{"https://k.example.com", "posters/x.jpg", "https://k.example.com/posters/x.jpg"},
		{"https://k.example.com", "https://cdn.example.com/x.jpg", "https://cdn.example.com/x.jpg"},
		{"", "/posters/x.jpg", ""},
		{"https://k.example.com", "", ""},
	}
	for _, c := range cases {
		got := AbsoluteImageURL(c.base, c.poster)
		if got != c.want {
			t.Errorf("AbsoluteImageURL(%q, %q) = %q, want %q",
				c.base, c.poster, got, c.want)
		}
	}
}

func TestFallbackDescriptionVenueAndDate(t *testing.T) {
	out := Render([]byte(stubIndex), Props{
		URL:      "https://kasa.example.com/event/x",
		Title:    "Standup",
		Venue:    "Atlas",
		StartsAt: time.Date(2026, 6, 7, 19, 0, 0, 0, time.UTC),
	})
	s := string(out)
	if !strings.Contains(s, "Atlas") {
		t.Errorf("venue missing from fallback description:\n%s", s)
	}
	if !strings.Contains(s, "червня") {
		t.Errorf("date missing from fallback description:\n%s", s)
	}
}
