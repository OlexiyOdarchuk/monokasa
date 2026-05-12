package token

import (
	"strings"
	"testing"
)

func TestNewCodeFormat(t *testing.T) {
	c := NewCoder("secret")
	seen := map[string]bool{}
	for range 200 {
		code, err := c.NewCode()
		if err != nil {
			t.Fatalf("NewCode: %v", err)
		}
		if len(code) != 8 {
			t.Fatalf("len(code)=%d want 8 (%q)", len(code), code)
		}
		if code != strings.ToLower(code) {
			t.Fatalf("code not lowercase: %q", code)
		}
		for _, r := range code {
			if !(r >= 'a' && r <= 'z') && !(r >= '2' && r <= '7') {
				t.Fatalf("code %q contains non-base32 char %q", code, r)
			}
		}
		if seen[code] {
			t.Fatalf("duplicate code in 200 draws: %q", code)
		}
		seen[code] = true
	}
}

func TestQRRoundtrip(t *testing.T) {
	c := NewCoder("the-secret")
	p := c.QRPayload(42, 7)
	rid, sid, err := c.VerifyQRPayload(p)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rid != 42 || sid != 7 {
		t.Fatalf("ids = (%d,%d) want (42,7)", rid, sid)
	}
}

func TestQRTamper(t *testing.T) {
	c := NewCoder("the-secret")
	p := c.QRPayload(42, 7)

	// Flip a char inside the body half.
	parts := strings.SplitN(p, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("payload not in 'body.sig' shape: %q", p)
	}
	badBody := "A" + parts[0][1:] + "." + parts[1]
	if _, _, err := c.VerifyQRPayload(badBody); err == nil {
		t.Fatal("tampered body should fail verification")
	}

	// Flip a char inside the signature half.
	badSig := parts[0] + "." + "A" + parts[1][1:]
	if _, _, err := c.VerifyQRPayload(badSig); err == nil {
		t.Fatal("tampered signature should fail verification")
	}
}

func TestQRWrongSecret(t *testing.T) {
	a := NewCoder("alpha")
	b := NewCoder("bravo")
	p := a.QRPayload(1, 2)
	if _, _, err := b.VerifyQRPayload(p); err == nil {
		t.Fatal("verify with wrong secret should fail")
	}
}

func TestQRMalformed(t *testing.T) {
	c := NewCoder("s")
	for _, in := range []string{"", "abc", "no-dot-here", "@@@.@@@", ".sig"} {
		if _, _, err := c.VerifyQRPayload(in); err == nil {
			t.Fatalf("expected error for %q", in)
		}
	}
}
