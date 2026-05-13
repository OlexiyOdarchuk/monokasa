// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	TGToken      string
	Secret       string
	JarLink      string
	ScannerToken string
	AdminTGID    int64
	HTTPAddr     string
	DBPath       string
	ShowTitle    string
	ShowVenue    string
	ShowStartsAt time.Time
	Rows, Cols   int
	PriceKopecks int64
	HoldDuration time.Duration
	RemindBefore time.Duration

	// MonoToken і WebhookURL — опційні. Якщо обидва задані, при старті
	// процес реєструє WebhookURL у monobank через /personal/webhook.
	// Інакше реєстрацію треба зробити вручну (одноразово).
	MonoToken  string
	WebhookURL string
}

func Load() (Config, error) {
	c := Config{
		HTTPAddr:     env("HTTP_ADDR", ":8090"),
		DBPath:       env("DB_PATH", "tix.db"),
		ShowTitle:    env("SHOW_TITLE", "Моя вистава"),
		ShowVenue:    env("SHOW_VENUE", "Театральна площа"),
		Rows:         envInt("ROWS", 5),
		Cols:         envInt("COLS", 6),
		PriceKopecks: int64(envInt("PRICE_KOPECKS", 25000)),
		HoldDuration: envDur("HOLD", 15*time.Minute),
		RemindBefore: envDur("REMIND_BEFORE", time.Hour),
	}
	c.TGToken = os.Getenv("TG_TOKEN")
	c.Secret = os.Getenv("TICKET_SECRET")
	c.JarLink = os.Getenv("MONO_JAR_LINK")
	c.ScannerToken = os.Getenv("SCANNER_TOKEN")
	c.MonoToken = os.Getenv("MONO_TOKEN")
	c.WebhookURL = os.Getenv("WEBHOOK_URL")
	c.AdminTGID, _ = strconv.ParseInt(os.Getenv("ADMIN_TG_ID"), 10, 64)

	if c.TGToken == "" {
		return c, errors.New("TG_TOKEN is required")
	}
	if c.Secret == "" {
		return c, errors.New("TICKET_SECRET is required")
	}
	if c.JarLink == "" {
		return c, errors.New("MONO_JAR_LINK is required")
	}

	t, err := time.Parse(time.RFC3339, env("SHOW_STARTS_AT", "2026-06-01T19:00:00+03:00"))
	if err != nil {
		return c, fmt.Errorf("SHOW_STARTS_AT: %w", err)
	}
	c.ShowStartsAt = t
	return c, nil
}

func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func envInt(k string, fallback int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envDur(k string, fallback time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
