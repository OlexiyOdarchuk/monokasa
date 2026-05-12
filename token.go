package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"strings"
)

// Coder produces deterministic, signed identifiers for reservations and tickets.
type Coder struct {
	secret []byte
}

func NewCoder(secret string) *Coder {
	return &Coder{secret: []byte(secret)}
}

// NewReservationCode returns a short, url/base32-safe random code that the user
// types into the mono jar payment comment. 8 chars (40 bits of entropy) is
// plenty given comments are scoped to one show.
func (c *Coder) NewReservationCode() (string, error) {
	var raw [5]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])), nil
}

// QRPayload returns a self-verifying ticket token of the form
// "<base64url(body)>.<base64url(hmac)>". body contains "<reservation_id>:<seat_id>".
// Use [VerifyQRPayload] on the scanning side.
func (c *Coder) QRPayload(reservationID, seatID int64) string {
	body := fmt.Sprintf("%d:%d", reservationID, seatID)
	mac := hmac.New(sha256.New, c.secret)
	mac.Write([]byte(body))
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString([]byte(body)) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
}

// VerifyQRPayload returns the embedded reservation_id and seat_id iff the
// signature checks out.
func (c *Coder) VerifyQRPayload(payload string) (reservationID, seatID int64, err error) {
	parts := strings.SplitN(payload, ".", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("malformed payload")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("body decode: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("sig decode: %w", err)
	}
	mac := hmac.New(sha256.New, c.secret)
	mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), sig) {
		return 0, 0, fmt.Errorf("signature mismatch")
	}
	if _, err := fmt.Sscanf(string(body), "%d:%d", &reservationID, &seatID); err != nil {
		return 0, 0, fmt.Errorf("body format: %w", err)
	}
	return reservationID, seatID, nil
}
