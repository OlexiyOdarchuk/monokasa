// Package token mints two kinds of identifiers: an 8-char human-typeable
// code that goes into the mono jar payment comment, and a self-verifying
// payload that gets embedded into the ticket QR.
package token

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"strings"
)

// Coder is the small HMAC-SHA256 verifier behind both code formats.
type Coder struct{ secret []byte }

func NewCoder(secret string) *Coder { return &Coder{secret: []byte(secret)} }

// NewCode returns an 8-char lower-case base32 reservation code (~40 bits of
// entropy). Short enough to type into a payment comment by hand.
func (c *Coder) NewCode() (string, error) {
	var raw [5]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])), nil
}

// QRPayload returns "<base64url(body)>.<base64url(hmac)>" where body is
// "<reservationID>:<seatID>". Use [Coder.VerifyQRPayload] on scan.
func (c *Coder) QRPayload(reservationID, seatID int64) string {
	body := fmt.Sprintf("%d:%d", reservationID, seatID)
	mac := hmac.New(sha256.New, c.secret)
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString([]byte(body)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

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
