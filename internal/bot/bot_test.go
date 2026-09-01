package bot

import (
	"net/url"
	"strings"
	"testing"

	"github.com/OlexiyOdarchuk/go-monobank-sdk/v2/currency"
	"github.com/OlexiyOdarchuk/go-monobank-sdk/v2/money"
)

func TestNormalizeName(t *testing.T) {
	t.Run("accepts", func(t *testing.T) {
		cases := []struct{ in, out string }{
			{"Олексій Одарчук", "Олексій Одарчук"},
			{"  Олексій   Одарчук  ", "Олексій Одарчук"},
			{"Анна-Марія О'Брайен", "Анна-Марія О'Брайен"},
			{"АБ", "АБ"},
			{"Іван\tСидоренко", "Іван Сидоренко"},
		}
		for _, c := range cases {
			got, err := normalizeName(c.in)
			if err != nil {
				t.Errorf("normalizeName(%q) errored: %v", c.in, err)
				continue
			}
			if got != c.out {
				t.Errorf("normalizeName(%q) = %q, want %q", c.in, got, c.out)
			}
		}
	})

	t.Run("rejects", func(t *testing.T) {
		bad := []string{
			"",
			"   ",
			"A",
			"  Я ",
			strings.Repeat("я", nameMaxRunes+1),
		}
		for _, in := range bad {
			if _, err := normalizeName(in); err == nil {
				t.Errorf("normalizeName(%q) should have failed", in)
			}
		}
	})
}

func TestJarPrefillURL(t *testing.T) {
	base := "https://send.monobank.ua/jar/abc"

	t.Run("round amount", func(t *testing.T) {
		got := jarPrefillURL(base, money.New(25000, currency.UAH), "abcdefgh")
		u, err := url.Parse(got)
		if err != nil {
			t.Fatal(err)
		}
		if a := u.Query().Get("a"); a != "250" {
			t.Errorf("a=%q want 250", a)
		}
		if c := u.Query().Get("t"); c != "abcdefgh" {
			t.Errorf("t=%q want abcdefgh", c)
		}
	})

	t.Run("non-round kopecks keep decimals", func(t *testing.T) {
		got := jarPrefillURL(base, money.New(25099, currency.UAH), "x")
		u, _ := url.Parse(got)
		if a := u.Query().Get("a"); a != "250.99" {
			t.Errorf("a=%q want 250.99", a)
		}
	})

	t.Run("invalid base returns input", func(t *testing.T) {
		got := jarPrefillURL("://not-a-url", money.New(100, currency.UAH), "x")
		if got != "://not-a-url" {
			t.Errorf("got %q, want passthrough", got)
		}
	})
}
