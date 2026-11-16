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

type kvStore interface {
	get(k string) string
	put(k string, v []byte)
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

// attemptRecord is what a retry of the same request id needs: the last
// PaymentIntent created for it and how many were created before it (the
// idempotency key's suffix — the key of a cancelled PaymentIntent would
// only replay that cancelled PaymentIntent).
type attemptRecord struct {
	PI  string `json:"pi"`
	Seq int    `json:"seq"`
}

// PaymentIntent outcomes this flow distinguishes.
const (
	piPaid      = "paid"
	piCancelled = "cancelled"
	piUnknown   = "unknown"
)

func (c *terminalCharger) elapsed() time.Duration { return c.clk.now().Sub(c.start) }

func (c *terminalCharger) attemptKey() string { return "attempt:" + c.requestID }

func (c *terminalCharger) loadAttempt() attemptRecord {
	var rec attemptRecord
	if c.requestID != "" {
		_ = json.Unmarshal([]byte(c.kv.get(c.attemptKey())), &rec)
	}
	return rec
}

func (c *terminalCharger) saveAttempt(rec attemptRecord) {
	if c.requestID == "" {
		return
	}
	b, _ := json.Marshal(rec)
	c.kv.put(c.attemptKey(), b)
}

func (c *terminalCharger) paid(pi string) outcome { return outcome{approved: true, authCode: pi} }

func refused(reason string) outcome { return outcome{reason: reason} }

func (c *terminalCharger) run() outcome {
	// 0. A retry of the same tender: settle the earlier PaymentIntent
	// first. A late tap that went through is this sale's payment.
	rec := c.loadAttempt()
	if rec.PI != "" {
		switch c.settle(rec.PI) {
		case piPaid:
			return c.paid(rec.PI)
		case piCancelled:
			rec = attemptRecord{Seq: rec.Seq + 1}
			c.saveAttempt(rec)
		default:
			return refused("outcome_unknown")
		}
	}

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
		return refused("network_error")
	}
	var pi struct {
		ID    string `json:"id"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &pi)
	if pi.ID == "" {
		return refused(firstNonEmpty(pi.Error.Code, "create_failed"))
	}
	rec.PI = pi.ID
	c.saveAttempt(rec)

	// 2. Tell the reader to collect + process the payment. If it refuses
	// (or we can't tell), cancel the PaymentIntent so nothing collects it
	// later.
	body, ok = c.api.call("POST", "/v1/terminal/readers/"+c.readerID+"/process_payment_intent", "payment_intent="+pi.ID, "")
	var rd struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if ok {
		_ = json.Unmarshal(body, &rd)
	}
	if !ok || rd.Error.Code != "" {
		switch c.settle(pi.ID) {
		case piPaid:
			return c.paid(pi.ID)
		case piCancelled:
			if !ok {
				return refused("network_error")
			}
			return refused(rd.Error.Code)
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
		switch c.status(pi.ID) {
		case "succeeded", "requires_capture":
			return c.paid(pi.ID)
		case "canceled":
			return refused("canceled")
		}
		c.clk.sleep(pollInterval)
	}

	// 5. Out of time: stop the reader, then report what really happened.
	switch c.settle(pi.ID) {
	case piPaid:
		return c.paid(pi.ID)
	case piCancelled:
		return refused("reader_timeout")
	}
	return refused("outcome_unknown")
}

// status reads a PaymentIntent's status; "" when the request failed.
func (c *terminalCharger) status(pi string) string {
	body, ok := c.api.call("GET", "/v1/payment_intents/"+pi, "", "")
	if !ok {
		return ""
	}
	var p struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(body, &p)
	return p.Status
}

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
// was: cancel the reader action and the PaymentIntent, then re-check until
// hardDeadline. A PaymentIntent that is processing (the card was tapped)
// refuses the cancel and resolves to succeeded or back to collectable.
func (c *terminalCharger) settle(pi string) string {
	if s := classify(c.status(pi)); s != piUnknown {
		return s
	}
	c.api.call("POST", "/v1/terminal/readers/"+c.readerID+"/cancel_action", "", "")
	cancelled := false
	for {
		if !cancelled {
			body, ok := c.api.call("POST", "/v1/payment_intents/"+pi+"/cancel", "", "")
			if ok {
				var p struct {
					Status string `json:"status"`
				}
				_ = json.Unmarshal(body, &p)
				cancelled = p.Status == "canceled"
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
