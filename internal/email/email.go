// Package email delivers the PDF ticket over SMTP for buyers who came in
// through the public web checkout (where Telegram isn't a delivery option).
//
// Self-host operators wire up any SMTP provider — Gmail/Workspace, Resend,
// Mailgun, Postmark, plain Postfix — via the SMTP_* env vars. The package
// uses net/smtp directly so there's zero extra runtime dependency.
//
// Composition is intentionally minimal MIME: multipart/mixed with one
// text/html part and one application/pdf attachment, base64-encoded with
// CRLF line breaks. The result parses cleanly in Gmail, Apple Mail and
// most Outlook generations.
package email

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"strings"
	"sync"
	"time"
)

// Sender is the abstraction the pay processor depends on. Implementations:
// SMTPSender for production; MockSender for tests.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// Message describes one outbound email. Zero, one, or many attachments
// are supported — buyer-side multi-seat orders need N PDFs in a single
// message; cancellation emails carry none.
type Message struct {
	To          string
	Subject     string
	HTMLBody    string
	Attachments []Attachment
}

// Attachment is one file payload inside a Message. ContentType defaults
// to "application/octet-stream" when empty; for PDFs pass "application/pdf".
type Attachment struct {
	Filename    string
	Body        []byte
	ContentType string
}

// SMTPSender uses standard net/smtp. STARTTLS is upgraded automatically by
// smtp.Client.StartTLS when the server advertises it. ImplicitTLS=true
// forces a TLS handshake before any SMTP commands (port 465 style).
type SMTPSender struct {
	host        string
	port        string
	username    string
	password    string
	from        string
	implicitTLS bool
	dialTimeout time.Duration
}

// Config holds everything needed to talk to an SMTP provider. From is
// the visible "From:" address; if your provider rewrites the envelope
// (Resend, Mailgun) you can set From to the verified address you want
// recipients to see.
type Config struct {
	Host        string
	Port        string
	Username    string
	Password    string
	From        string
	ImplicitTLS bool // true for port 465; false for 587/STARTTLS or plain
}

func NewSMTPSender(c Config) (*SMTPSender, error) {
	if c.Host == "" || c.From == "" {
		return nil, errors.New("smtp: host and from are required")
	}
	if c.Port == "" {
		c.Port = "587"
	}
	return &SMTPSender{
		host: c.Host, port: c.Port,
		username: c.Username, password: c.Password,
		from: c.From, implicitTLS: c.ImplicitTLS,
		dialTimeout: 15 * time.Second,
	}, nil
}

func (s *SMTPSender) Send(ctx context.Context, msg Message) error {
	if msg.To == "" {
		return errors.New("smtp: empty To address")
	}
	raw, err := BuildMessage(s.from, msg)
	if err != nil {
		return fmt.Errorf("build message: %w", err)
	}

	addr := net.JoinHostPort(s.host, s.port)

	// Honour ctx.Deadline by giving the dial that budget when present.
	dialer := &net.Dialer{Timeout: s.dialTimeout}
	if deadline, ok := ctx.Deadline(); ok {
		dialer.Deadline = deadline
	}

	var conn net.Conn
	if s.implicitTLS {
		// Port 465 / "SMTPS" — TLS from the first byte.
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: s.host})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("smtp dial %s: %w", addr, err)
	}
	defer conn.Close()

	cli, err := smtp.NewClient(conn, s.host)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer cli.Quit()

	// STARTTLS for plain connections that advertise it (587 / 25 with TLS).
	if !s.implicitTLS {
		if ok, _ := cli.Extension("STARTTLS"); ok {
			if err := cli.StartTLS(&tls.Config{ServerName: s.host}); err != nil {
				return fmt.Errorf("starttls: %w", err)
			}
		}
	}

	if s.username != "" || s.password != "" {
		auth := smtp.PlainAuth("", s.username, s.password, s.host)
		if err := cli.Auth(auth); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
	}

	if err := cli.Mail(s.from); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	if err := cli.Rcpt(msg.To); err != nil {
		return fmt.Errorf("RCPT TO: %w", err)
	}
	w, err := cli.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close body: %w", err)
	}
	return nil
}

// BuildMessage assembles the raw SMTP DATA payload (headers + multipart
// body) for a message with one HTML part and one PDF attachment.
//
// Lines are CRLF-terminated, which SMTP RFCs require. The boundary is a
// fixed string because we don't ship multiple attachments; if we ever do,
// switch to mime/multipart.Writer.
func BuildMessage(from string, msg Message) ([]byte, error) {
	if msg.To == "" || from == "" {
		return nil, errors.New("from and To required")
	}
	const boundary = "monokasa-mime-boundary-7d3f"

	var buf bytes.Buffer
	w := func(s string) { buf.WriteString(s); buf.WriteString("\r\n") }

	w("From: " + from)
	w("To: " + msg.To)
	w("Subject: " + encodeHeader(msg.Subject))
	w("Date: " + time.Now().UTC().Format(time.RFC1123Z))
	w("MIME-Version: 1.0")
	w("Content-Type: multipart/mixed; boundary=" + boundary)
	w("")

	// HTML body
	w("--" + boundary)
	w("Content-Type: text/html; charset=utf-8")
	w("Content-Transfer-Encoding: quoted-printable")
	w("")
	if _, err := io.Copy(&buf, quotedPrintable(strings.NewReader(msg.HTMLBody))); err != nil {
		return nil, err
	}
	w("")

	// Each attachment as its own multipart part — base64 body wrapped at
	// 76 chars per RFC 2045.
	for _, att := range msg.Attachments {
		if att.Filename == "" || len(att.Body) == 0 {
			continue
		}
		ct := att.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		w("--" + boundary)
		w("Content-Type: " + ct + "; name=" + quoteHeader(att.Filename))
		w("Content-Transfer-Encoding: base64")
		w(`Content-Disposition: attachment; filename=` + quoteHeader(att.Filename))
		w("")
		writeBase64Wrapped(&buf, att.Body)
		w("")
	}

	w("--" + boundary + "--")
	return buf.Bytes(), nil
}

// encodeHeader wraps non-ASCII headers in RFC 2047 encoded-word form so
// "Твій квиток" doesn't arrive as `=?UTF-8?B?...?=` mojibake.
func encodeHeader(s string) string {
	for _, r := range s {
		if r > 0x7e {
			return mime.QEncoding.Encode("utf-8", s)
		}
	}
	return s
}

// quoteHeader wraps a filename in double quotes, escaping internal quotes.
// Sufficient for our use (PDF filenames we generate ourselves).
func quoteHeader(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// quotedPrintable wraps an io.Reader so the bytes pass through
// mime/quotedprintable on the way out. Used for the HTML body to keep
// non-ASCII characters (Ukrainian, em-dashes, smart quotes) readable in
// SMTP servers that strip the 8th bit.
func quotedPrintable(r io.Reader) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		qpW := quotedprintable.NewWriter(pw)
		_, err := io.Copy(qpW, r)
		_ = qpW.Close()
		_ = pw.CloseWithError(err)
	}()
	return pr
}

// writeBase64Wrapped emits base64 with CRLF every 76 chars per RFC 2045.
func writeBase64Wrapped(w io.Writer, data []byte) {
	enc := base64.StdEncoding.EncodeToString(data)
	const lineLen = 76
	for i := 0; i < len(enc); i += lineLen {
		end := min(i+lineLen, len(enc))
		_, _ = w.Write([]byte(enc[i:end]))
		_, _ = w.Write([]byte("\r\n"))
	}
}

// --- MockSender (for tests, exported so adapters can satisfy Sender) ---

// MockSender collects every message it's asked to send. Goroutine-safe.
type MockSender struct {
	mu   sync.Mutex
	Sent []Message
	Err  error // if set, Send returns this without recording
}

func (m *MockSender) Send(_ context.Context, msg Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return m.Err
	}
	m.Sent = append(m.Sent, msg)
	return nil
}
