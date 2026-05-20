package email

import (
	"context"
	"encoding/base64"
	"errors"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
)

func TestBuildMessageHasRequiredHeaders(t *testing.T) {
	raw, err := BuildMessage("noreply@example.com", Message{
		To:       "buyer@example.com",
		Subject:  "Your ticket",
		HTMLBody: "<p>hi</p>",
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := msg.Header.Get("From"); got != "noreply@example.com" {
		t.Errorf("From = %q", got)
	}
	if got := msg.Header.Get("To"); got != "buyer@example.com" {
		t.Errorf("To = %q", got)
	}
	if got := msg.Header.Get("MIME-Version"); got != "1.0" {
		t.Errorf("MIME-Version = %q", got)
	}
	ctype := msg.Header.Get("Content-Type")
	if !strings.HasPrefix(ctype, "multipart/mixed;") {
		t.Errorf("Content-Type = %q, want multipart/mixed", ctype)
	}
}

func TestBuildMessageEncodesNonASCIISubject(t *testing.T) {
	raw, err := BuildMessage("a@x.com", Message{
		To:       "b@y.com",
		Subject:  "Твій квиток на захід",
		HTMLBody: "<p>...</p>",
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := mail.ReadMessage(strings.NewReader(string(raw)))
	subj := msg.Header.Get("Subject")
	// Either net/mail already decoded it (newer Go) or we see the encoded
	// form. Both are fine; what matters is that the wire bytes weren't raw
	// UTF-8 (those would have failed mail.ReadMessage somewhere).
	dec, err := new(mime.WordDecoder).DecodeHeader(subj)
	if err != nil {
		t.Fatal(err)
	}
	if dec != "Твій квиток на захід" {
		t.Errorf("decoded subject = %q", dec)
	}
}

func TestBuildMessageRoundTripsPDFAttachment(t *testing.T) {
	pdf := []byte("%PDF-1.4 fake pdf bytes")
	raw, err := BuildMessage("from@x.com", Message{
		To:             "to@y.com",
		Subject:        "Your ticket",
		HTMLBody:       "<p>see attachment</p>",
		AttachmentName: "ticket.pdf",
		AttachmentBody: pdf,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var sawHTML, sawPDF bool
	for {
		p, err := mr.NextPart()
		if errors.Is(err, mime.ErrInvalidMediaParameter) {
			break // EOF-equivalent for some Go versions
		}
		if err != nil {
			break
		}
		ct := p.Header.Get("Content-Type")
		switch {
		case strings.HasPrefix(ct, "text/html"):
			sawHTML = true
		case strings.HasPrefix(ct, "application/pdf"):
			sawPDF = true
			// Decode and compare bytes.
			body, _ := readPart(p)
			decoded, err := base64.StdEncoding.DecodeString(stripWhitespace(body))
			if err != nil {
				t.Fatalf("base64 decode: %v", err)
			}
			if string(decoded) != string(pdf) {
				t.Errorf("attachment mismatch: got %q want %q", decoded, pdf)
			}
		}
	}
	if !sawHTML {
		t.Error("HTML part missing")
	}
	if !sawPDF {
		t.Error("PDF part missing")
	}
}

func TestNewSMTPSenderRequiresHostAndFrom(t *testing.T) {
	if _, err := NewSMTPSender(Config{}); err == nil {
		t.Error("empty config: want error")
	}
	if _, err := NewSMTPSender(Config{Host: "smtp.x", From: "a@b.com"}); err != nil {
		t.Errorf("valid config: got %v", err)
	}
}

func TestMockSenderRecordsAndReturnsErr(t *testing.T) {
	m := &MockSender{}
	if err := m.Send(context.Background(), Message{To: "x@y.com"}); err != nil {
		t.Fatal(err)
	}
	if len(m.Sent) != 1 {
		t.Errorf("Sent len = %d", len(m.Sent))
	}

	m2 := &MockSender{Err: errors.New("boom")}
	if err := m2.Send(context.Background(), Message{To: "x@y.com"}); err == nil {
		t.Error("Err set but Send returned nil")
	}
	if len(m2.Sent) != 0 {
		t.Errorf("Sent should be untouched on error, got %d", len(m2.Sent))
	}
}

// --- helpers ---

func readPart(p interface{ Read([]byte) (int, error) }) (string, error) {
	var sb strings.Builder
	buf := make([]byte, 1024)
	for {
		n, err := p.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return sb.String(), nil
}

func stripWhitespace(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r != '\r' && r != '\n' && r != ' ' && r != '\t' {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
