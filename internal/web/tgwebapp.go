package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// VerifyTelegramInitData validates the signed `initData` blob Telegram
// hands a Web App on load. Returns the authenticated Telegram user id on
// success, or an error explaining why the signature didn't check out.
//
// Format reference: https://core.telegram.org/bots/webapps#validating-data-received-via-the-web-app
//
// Algorithm:
//  1. Parse `initData` as a URL-encoded query string.
//  2. Pull the `hash` field aside (the value the server should reproduce).
//  3. Sort the remaining key=value pairs alphabetically by key, join with '\n'.
//  4. secret_key = HMAC-SHA256(key="WebAppData", data=botToken)
//  5. expected_hash = hex(HMAC-SHA256(key=secret_key, data=sortedString))
//  6. Compare expected_hash to the provided `hash` in constant time.
//  7. Parse the JSON `user` field and return user.id.
func VerifyTelegramInitData(initData, botToken string) (userID int64, err error) {
	values, err := url.ParseQuery(initData)
	if err != nil {
		return 0, fmt.Errorf("parse initData: %w", err)
	}
	providedHash := values.Get("hash")
	if providedHash == "" {
		return 0, fmt.Errorf("initData missing hash")
	}
	values.Del("hash")

	keys := make([]string, 0, len(values))
	for k := range values {
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
		sb.WriteString(values.Get(k))
	}

	secretMac := hmac.New(sha256.New, []byte("WebAppData"))
	secretMac.Write([]byte(botToken))
	secret := secretMac.Sum(nil)

	checkMac := hmac.New(sha256.New, secret)
	checkMac.Write([]byte(sb.String()))
	expected := hex.EncodeToString(checkMac.Sum(nil))

	if !hmac.Equal([]byte(expected), []byte(providedHash)) {
		return 0, fmt.Errorf("initData signature mismatch")
	}

	// user is a JSON object: {"id":123,"first_name":"...",...}
	userJSON := values.Get("user")
	if userJSON == "" {
		return 0, fmt.Errorf("initData has no user")
	}
	var user struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(userJSON), &user); err != nil {
		return 0, fmt.Errorf("parse user: %w", err)
	}
	if user.ID == 0 {
		return 0, fmt.Errorf("user.id missing")
	}
	return user.ID, nil
}

// _ keeps strconv imported so future int field extraction (auth_date etc)
// doesn't require touching the import block. Cheap and explicit.
var _ = strconv.Itoa
