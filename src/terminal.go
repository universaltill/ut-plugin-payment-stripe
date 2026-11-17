package main

// The Stripe Terminal (card-present) authorize flow. No //go:build tag: it
// talks to Stripe, the clock and plugin storage only through the small
// interfaces below, so `go test` drives it with fakes (terminal_test.go)
// while main.go wires the real host functions in the wasip1 build.
//
// Why the deadlines (ut-docs#6304): universal-till gives a payment
// authorize call 30 s (paymentGateTimeout in internal/plugins/
// wasm_runtime.go), then kills the plugin and shows the sale unpaid. A
// reader left waiting past that point would still take a late tap, and the
// customer would be charged for a sale the till shows unpaid. So the flow
// gives the customer pollBudget to tap, then cancels the reader action AND
// the PaymentIntent (a cancelled PaymentIntent can never be charged), then
// re-checks until hardDeadline — all inside the gate, measured from the
// plugin's own start (time spent queueing for a call slot also counts
// against the host's 30 s, hence the margin). If the payment went through
// anyway it is approved, never dropped. If the outcome is still unknown
// the decline says so (outcome_unknown) and the PaymentIntent stays on
// record under the till's request id: the till sends the SAME id when the
// cashier taps Pay again on the same basket (ut-docs#1762), and the retry
// resolves that PaymentIntent before it would ever start a second charge.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	pollBudget   = 18 * time.Second // customer's time to tap
	hardDeadline = 24 * time.Second // last call starts by now; host gate is 30 s, and a slow call or cold start eats the rest
	pollInterval = time.Second
)

// stripeAPI performs one Stripe request. ok is false only on a transport
// failure; an API error still returns its body. idemKey, when not empty,
// is sent as the Idempotency-Key header.
type stripeAPI interface {
	call(method, path, form, idemKey string) (body []byte, ok bool)
}

type clock interface {
	now() time.Time
	sleep(time.Duration)
}

// outcome is the flow's verdict; main.go turns it into approve()/decline().
type outcome struct {
	approved bool
	authCode string // PaymentIntent id when approved
	reason   string // decline code otherwise
}

type terminalCharger struct {
	api       stripeAPI
	clk       clock
	kv        kvStore
	sk        string
	currency  string
	readerID  string
	requestID string // the till's authorize event id; "" on an older till
	amount    int64
	start     time.Time
}

// PaymentIntent outcomes this flow distinguishes.
const (
	piPaid      = "paid"
	piCancelled = "cancelled"
	piUnknown   = "unknown"
)

// retryStartBudget: settling an earlier attempt can take most of the gate;
// past this point a fresh charge would be cancelled almost at once, so the
// retry declines and the cashier's next tap starts clean.
const retryStartBudget = 6 * time.Second

func (c *terminalCharger) elapsed() time.Duration { return c.clk.now().Sub(c.start) }

func (c *terminalCharger) paid(pi string) outcome { return outcome{approved: true, authCode: pi} }

func refused(reason string) outcome { return outcome{reason: reason} }

// stripeError is the error object of a Stripe API response; present when
// any of its fields is set (some errors carry no code).
type stripeError struct {
	Code    string `json:"code"`
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (e stripeError) present() bool { return e.Code != "" || e.Type != "" || e.Message != "" }

func (c *terminalCharger) run() outcome {
	// 0. A retry of the same tender: settle the earlier PaymentIntent
	// first. A late tap that went through is this sale's payment.
	rec, found := findAttempt(c.kv, c.requestID)
	if found && rec.Ref != "" {
		switch c.settle(rec.Ref, true, true) {
		case piPaid:
			return c.paid(rec.Ref)
		case piCancelled:
			rec = attemptEntry{ID: c.requestID, Seq: rec.Seq + 1}
			putAttempt(c.kv, rec)
			if c.elapsed() > retryStartBudget {
				return refused("previous_attempt_settled")
			}
		default:
			return refused("outcome_unknown")
		}
	}
	rec.ID = c.requestID

	// 1. Create a card_present PaymentIntent (auto-captured on success).
	idem := ""
	if c.requestID != "" {
		idem = fmt.Sprintf("ut-%s-%d", c.requestID, rec.Seq)
	}
	form := fmt.Sprintf("amount=%d&currency=%s&payment_method_types[]=card_present&capture_method=automatic", c.amount, c.currency)
	if c.requestID != "" {
		form += "&metadata[ut_request_id]=" + url.QueryEscape(c.requestID)
	}
	body, ok := c.api.call("POST", "/v1/payment_intents", form, idem)
	if !ok {
		// Keep the key: Stripe returns the same PaymentIntent if this
		// request did create one.
		return refused("network_error")
	}
	var pi struct {
		ID    string      `json:"id"`
		Error stripeError `json:"error"`
	}
	_ = json.Unmarshal(body, &pi)
	if pi.ID == "" {
		// Stripe replays a stored error for the same Idempotency-Key for
		// 24 h; move on to a new key so a retry can succeed. An unprocessed
		// card_present PaymentIntent can't be charged, so this is safe.
		putAttempt(c.kv, attemptEntry{ID: c.requestID, Seq: rec.Seq + 1})
		return refused(firstNonEmpty(pi.Error.Code, "create_failed"))
	}
	rec.Ref = pi.ID
	rec.At = c.clk.now().Unix()
	putAttempt(c.kv, rec)

	// 2. Tell the reader to collect + process the payment.
	body, ok = c.api.call("POST", "/v1/terminal/readers/"+c.readerID+"/process_payment_intent", "payment_intent="+pi.ID, "")
	var rd struct {
		Error stripeError `json:"error"`
	}
	if ok {
		_ = json.Unmarshal(body, &rd)
	}
	if !ok || rd.Error.present() {
		// The reader refused (or we can't tell): cancel the PaymentIntent so
		// nothing collects it later. A definite refusal means this reader is
		// not showing our payment — never cancel_action whatever it is doing.
		// terminal_reader_timeout is Stripe's documented false negative: the
		// reader may have the payment, so stop its prompt too.
		switch c.settle(pi.ID, false, !ok || rd.Error.Code == "terminal_reader_timeout") {
		case piPaid:
			return c.paid(pi.ID)
		case piCancelled:
			if !ok {
				return refused("network_error")
			}
			return refused(firstNonEmpty(rd.Error.Code, rd.Error.Type, "reader_error"))
		default:
			return refused("outcome_unknown")
		}
	}

	// 3. Test mode: present a simulated card so the PI can complete.
	if strings.HasPrefix(c.sk, "sk_test_") {
		c.api.call("POST", "/v1/test_helpers/terminal/readers/"+c.readerID+"/present_payment_method", "", "")
	}

	// 4. Give the customer pollBudget to tap.
	for c.elapsed() < pollBudget {
		st, declined := c.state(pi.ID)
		switch classify(st) {
		case piPaid:
			return c.paid(pi.ID)
		case piCancelled:
			return refused("canceled")
		}
		if declined != "" && st == "requires_payment_method" && c.readerActionFailed() {
			// The card was declined and the reader gave up on this payment
			// (action.status failed; ut-docs#6440): stop it and cancel, then
			// report Stripe's reason now instead of waiting out the poll. A
			// soft decline the reader is still handling (chip-and-PIN
			// prompt) has an action still in progress and keeps polling. A
			// tap that slipped in is still approved.
			switch c.settle(pi.ID, false, true) {
			case piPaid:
				return c.paid(pi.ID)
			case piCancelled:
				return refused(declined)
			}
			return refused("outcome_unknown")
		}
		c.clk.sleep(pollInterval)
	}

	// 5. Out of time: stop the reader, then report what really happened.
	// The poll just read the status, so go straight to cancelling.
	switch c.settle(pi.ID, false, true) {
	case piPaid:
		return c.paid(pi.ID)
	case piCancelled:
		return refused("reader_timeout")
	}
	return refused("outcome_unknown")
}

// readerActionFailed reports whether the reader's current action ended
// failed (GET /v1/terminal/readers/{id}, action.status).
func (c *terminalCharger) readerActionFailed() bool {
	body, ok := c.api.call("GET", "/v1/terminal/readers/"+c.readerID, "", "")
	if !ok {
		return false
	}
	var r struct {
		Action *struct {
			Status string `json:"status"`
		} `json:"action"`
	}
	_ = json.Unmarshal(body, &r)
	return r.Action != nil && r.Action.Status == "failed"
}

// status reads a PaymentIntent's status; "" when the request failed.
func (c *terminalCharger) status(pi string) string {
	st, _ := c.state(pi)
	return st
}

// state reads a PaymentIntent's status plus, when the last attempt on the
// reader was declined, Stripe's reason (decline_code, else code) from
// last_payment_error; "" when none or the request failed.
func (c *terminalCharger) state(pi string) (status, declined string) {
	body, ok := c.api.call("GET", "/v1/payment_intents/"+pi, "", "")
	if !ok {
		return "", ""
	}
	var p struct {
		Status string `json:"status"`
		Last   *struct {
			Code        string `json:"code"`
			DeclineCode string `json:"decline_code"`
		} `json:"last_payment_error"`
	}
	_ = json.Unmarshal(body, &p)
	if p.Last != nil {
		declined = firstNonEmpty(p.Last.DeclineCode, p.Last.Code, "card_declined")
	}
	return p.Status, declined
}

// classify maps a PaymentIntent status to this flow's outcomes.
// requires_capture counts as paid only because every PaymentIntent here is
// created with capture_method=automatic; manual capture would need a
// capture step before approving.
func classify(status string) string {
	switch status {
	case "succeeded", "requires_capture":
		return piPaid
	case "canceled":
		return piCancelled
	}
	return piUnknown
}

// settle makes sure pi can no longer be charged, or finds out it already
// was: cancel the reader action (when cancelReader) and the PaymentIntent,
// then re-check until hardDeadline. A PaymentIntent that is processing
// (the card was tapped) refuses the cancel and resolves to succeeded or
// back to collectable. readFirst: check the status before cancelling.
func (c *terminalCharger) settle(pi string, readFirst, cancelReader bool) string {
	if readFirst {
		if s := classify(c.status(pi)); s != piUnknown {
			return s
		}
	}
	if cancelReader {
		c.api.call("POST", "/v1/terminal/readers/"+c.readerID+"/cancel_action", "", "")
	}
	for {
		// Re-sent each round: a processing PaymentIntent refuses the cancel,
		// but one whose card was declined goes back to collectable.
		body, ok := c.api.call("POST", "/v1/payment_intents/"+pi+"/cancel", "", "")
		if ok {
			var p struct {
				Status string `json:"status"`
			}
			_ = json.Unmarshal(body, &p)
			if p.Status == "canceled" {
				return piCancelled
			}
		}
		if s := classify(c.status(pi)); s != piUnknown {
			return s
		}
		// Never start a call that could still be running when the host's
		// gate closes: a verdict the till never receives is lost.
		if c.elapsed()+pollInterval >= hardDeadline {
			return piUnknown
		}
		c.clk.sleep(pollInterval)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
