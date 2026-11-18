package main

import (
	"encoding/json"
	"net/url"
	"strconv"
	"testing"
)

// ledgerStripe behaves like Stripe's documented /v1/refunds: an amount above
// the unrefunded remainder is refused, and a request repeating an
// Idempotency-Key returns the first answer without refunding again.
// loseReplies makes the next N calls succeed AT STRIPE while the plugin sees
// a transport failure (reply lost).
type ledgerStripe struct {
	captured    map[string]int64
	refunded    map[string]int64
	byKey       map[string][]byte
	idemKeys    []string
	loseReplies int
	n           int
}

func newLedger(captured map[string]int64) *ledgerStripe {
	return &ledgerStripe{captured: captured, refunded: map[string]int64{}, byKey: map[string][]byte{}}
}

func (l *ledgerStripe) call(method, path, form, idemKey string) ([]byte, int, bool) {
	l.idemKeys = append(l.idemKeys, idemKey)
	q, _ := url.ParseQuery(form)
	pi := q.Get("payment_intent")
	amt, _ := strconv.ParseInt(q.Get("amount"), 10, 64)
	var body []byte
	if prev, ok := l.byKey[idemKey]; ok && idemKey != "" {
		body = prev
	} else {
		left := l.captured[pi] - l.refunded[pi]
		switch {
		case left == 0:
			body = []byte(`{"error":{"code":"charge_already_refunded"}}`)
		case amt > left:
			body = []byte(`{"error":{"code":"amount_too_large"}}`)
		default:
			l.refunded[pi] += amt
			l.n++
			body, _ = json.Marshal(map[string]any{"id": "re_" + strconv.Itoa(l.n), "status": "succeeded"})
		}
		if idemKey != "" {
			l.byKey[idemKey] = body
		}
	}
	if l.loseReplies > 0 {
		l.loseReplies--
		return nil, 0, false
	}
	return body, 200, true
}

func refundEvent(amount int64, legs ...origPayment) eventPayload {
	return eventPayload{Amount: amount, OriginalSaleID: "sale-1", OriginalPayments: legs}
}

func TestRefundSendsIdempotencyKeyFromEventAndPaymentIntent(t *testing.T) {
	api := newLedger(map[string]int64{"pi_sale": 1250})
	p := refundEvent(500, origPayment{TransactionRef: "pi_sale", Amount: 1250})
	p.EventID = "evt-42"
	if _, reason := refundSale(api, memKV{}, linkStore{blobs: newFakeBlobs()}, p); reason != "" {
		t.Fatal(reason)
	}
	if len(api.idemKeys) != 1 || api.idemKeys[0] != "ut-refund-evt-42-pi_sale" {
		t.Fatalf("idempotency keys: %q", api.idemKeys)
	}
}

func TestPartialRefundLostReplyIsNotRefundedTwice(t *testing.T) {
	api := newLedger(map[string]int64{"pi_sale": 1250})
	api.loseReplies = 1
	p := refundEvent(500, origPayment{TransactionRef: "pi_sale", Amount: 1250})
	p.EventID = "evt-1"
	kv, links := memKV{}, linkStore{blobs: newFakeBlobs()}
	_, r1 := refundSale(api, kv, links, p)
	id, r2 := refundSale(api, kv, links, p)
	if r1 != "network_error" || r2 != "" || id == "" {
		t.Fatalf("got %q / %q / %q", r1, r2, id)
	}
	if api.refunded["pi_sale"] != 500 {
		t.Fatalf("refunded %d, want 500 (no double refund)", api.refunded["pi_sale"])
	}
}

func TestFullRefundLostReplyRetryIsRecordable(t *testing.T) {
	api := newLedger(map[string]int64{"pi_sale": 1250})
	api.loseReplies = 1
	p := refundEvent(1250, origPayment{TransactionRef: "pi_sale", Amount: 1250})
	p.EventID = "evt-1"
	kv, links := memKV{}, linkStore{blobs: newFakeBlobs()}
	refundSale(api, kv, links, p)
	id, reason := refundSale(api, kv, links, p)
	if reason != "" || id == "" {
		t.Fatalf("retry should approve with the original refund, got %q / %q", id, reason)
	}
	if api.refunded["pi_sale"] != 1250 {
		t.Fatalf("refunded %d", api.refunded["pi_sale"])
	}
}

func TestLostReplyRetryDoesNotMoveOnToTheOtherLeg(t *testing.T) {
	api := newLedger(map[string]int64{"pi_A": 2500, "pi_B": 2500})
	api.loseReplies = 1
	p := refundEvent(2500, origPayment{TransactionRef: "pi_A", Amount: 2500}, origPayment{TransactionRef: "pi_B", Amount: 2500})
	p.EventID = "evt-1"
	kv, links := memKV{}, linkStore{blobs: newFakeBlobs()}
	refundSale(api, kv, links, p)
	refundSale(api, kv, links, p)
	if got := api.refunded["pi_A"] + api.refunded["pi_B"]; got != 2500 {
		t.Fatalf("refunded %d in total for a 2500 refund", got)
	}
}
