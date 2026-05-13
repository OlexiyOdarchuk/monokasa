// Package metrics centralises the expvar counters and gauges monokasa
// exposes on /debug/vars. Other packages bump these via the small typed
// helpers below; only main wires up the gauges that need to read from
// the store at scrape time.
package metrics

import "expvar"

var (
	// TicketsIssued counts confirmations end-to-end (webhook + reconcile).
	TicketsIssued = expvar.NewInt("monokasa_tickets_issued")

	// PaymentsWebhook counts confirmations that came through the live
	// monobank webhook path.
	PaymentsWebhook = expvar.NewInt("monokasa_payments_webhook")

	// PaymentsReconcile counts confirmations that came through /reconcile —
	// each one represents a webhook event Mono dropped, so a non-zero value
	// here is a "your webhook is flaky" signal.
	PaymentsReconcile = expvar.NewInt("monokasa_payments_reconcile")

	// WebhookErrors counts webhook-handler errors (signature mismatch,
	// processor failure, etc).
	WebhookErrors = expvar.NewInt("monokasa_webhook_errors")

	// ScansOK / ScansUsed / ScansInvalid count the three terminal states
	// of /scan/check, useful for spotting brute-force probes and for
	// "how many people actually walked in" after the show.
	ScansOK      = expvar.NewInt("monokasa_scans_ok")
	ScansUsed    = expvar.NewInt("monokasa_scans_used")
	ScansInvalid = expvar.NewInt("monokasa_scans_invalid")
)

// IssuedFromWebhook bumps both the total and the webhook-path counter.
func IssuedFromWebhook() {
	TicketsIssued.Add(1)
	PaymentsWebhook.Add(1)
}

// IssuedFromReconcile bumps both the total and the reconcile-path counter.
func IssuedFromReconcile() {
	TicketsIssued.Add(1)
	PaymentsReconcile.Add(1)
}
