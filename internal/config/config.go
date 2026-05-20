// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
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
	MonoToken    string
	WebhookURL   string

	// AdminEmail/AdminPassword bootstrap the first admin user on a fresh
	// install. Once any user exists in the DB these are ignored, so they
	// can be safely removed from .env after first start.
	AdminEmail    string
	AdminPassword string
	// SecureCookies forces Secure=true on session cookies regardless of
	// the request's TLS detection. Set in production behind HTTPS where
	// the proxy may or may not expose X-Forwarded-Proto.
	SecureCookies bool

	// SMTP: optional. When SMTPHost+SMTPFrom are set, web-buyer
	// reservations get a PDF emailed after payment. Without these the
	// reservation still confirms; operator just sees a warn log.
	SMTPHost        string
	SMTPPort        string
	SMTPUser        string
	SMTPPass        string
	SMTPFrom        string
	SMTPImplicitTLS bool

	// BotUsername is the @-handle of the Telegram bot (without "@").
	// When set, the public buyer page renders a "Connect Telegram"
	// button on the success screen, deep-linking t.me/<BotUsername>?
	// start=res_<code>. Without it, the button is hidden — bot still
	// works for /seats users.
	BotUsername string
}

func Load() (Config, error) {
	c := Config{
		HTTPAddr:     env("HTTP_ADDR", ":8093"),
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
	c.AdminEmail = os.Getenv("ADMIN_EMAIL")
	c.AdminPassword = os.Getenv("ADMIN_PASSWORD")
	c.SecureCookies = envBool("SECURE_COOKIES", false)
	c.SMTPHost = os.Getenv("SMTP_HOST")
	c.SMTPPort = env("SMTP_PORT", "587")
	c.SMTPUser = os.Getenv("SMTP_USER")
	c.SMTPPass = os.Getenv("SMTP_PASS")
	c.SMTPFrom = os.Getenv("SMTP_FROM")
	c.SMTPImplicitTLS = envBool("SMTP_IMPLICIT_TLS", false)
	c.BotUsername = strings.TrimPrefix(os.Getenv("BOT_USERNAME"), "@")

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

func envBool(k string, fallback bool) bool {
	switch strings.ToLower(os.Getenv(k)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return fallback
}
