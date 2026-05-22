package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
	"testing"
)

// signInitData mints a valid initData string for the given bot token —
// used in tests so we don't depend on Telegram to produce known-good
// signatures. Mirrors the algorithm in VerifyTelegramInitData.
func signInitData(t *testing.T, token string, fields map[string]string) string {
	t.Helper()
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(fields[k])
	}
	sm := hmac.New(sha256.New, []byte("WebAppData"))
	sm.Write([]byte(token))
	cm := hmac.New(sha256.New, sm.Sum(nil))
	cm.Write([]byte(sb.String()))
	hash := hex.EncodeToString(cm.Sum(nil))
	q := url.Values{}
	for k, v := range fields {
		q.Set(k, v)
	}
	q.Set("hash", hash)
	return q.Encode()
}

func TestVerifyTelegramInitDataOK(t *testing.T) {
	token := "12345:fake-bot-token"
	initData := signInitData(t, token, map[string]string{
		"auth_date": "1700000000",
		"user":      `{"id":42,"first_name":"Admin"}`,
	})
	uid, err := VerifyTelegramInitData(initData, token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if uid != 42 {
		t.Errorf("user id = %d, want 42", uid)
	}
}

func TestVerifyTelegramInitDataBadHash(t *testing.T) {
	token := "12345:fake-bot-token"
	initData := signInitData(t, token, map[string]string{
		"user": `{"id":1}`,
	})
	// Different token = different secret = signature mismatch.
	if _, err := VerifyTelegramInitData(initData, "other-token"); err == nil {
		t.Errorf("expected mismatch error")
	}
}

func TestVerifyTelegramInitDataNoHash(t *testing.T) {
	if _, err := VerifyTelegramInitData("user=foo", "tok"); err == nil {
		t.Errorf("expected missing-hash error")
	}
}

func TestVerifyTelegramInitDataNoUser(t *testing.T) {
	token := "tok"
	initData := signInitData(t, token, map[string]string{"auth_date": "1"})
	if _, err := VerifyTelegramInitData(initData, token); err == nil {
		t.Errorf("expected no-user error")
	}
}
