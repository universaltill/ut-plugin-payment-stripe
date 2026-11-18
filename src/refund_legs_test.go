package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func i64(v int64) *int64 { return &v }

func legPay(total int64, evt string, legs ...origPayment) eventPayload {
	p := refundEvent(total, legs...)
	p.EventID = evt
	return p
}

func leg(ref string, amount int64, refund *int64) origPayment {
	return origPayment{TransactionRef: ref, Amount: amount, RefundAmount: refund}
}

func captureLogs(t *testing.T) *[]string {
	var logs []string
	logHook = func(m string) { logs = append(logs, m) }
	t.Cleanup(func() { logHook = nil })
	return &logs
}

func noLinks() linkStore { return linkStore{blobs: newFakeBlobs()} }

func TestLegsAbsentUnchanged(t *testing.T) {
	api := newLedger(map[string]int64{"pi_A": 3000, "pi_B": 2000})
	p := legPay(2000, "evt-1", leg("pi_A", 3000, nil), leg("pi_B", 2000, nil))
	if _, d := refundSale(api, memKV{}, noLinks(), p); d != "" {
		t.Fatal(d)
	}
	// big-first fallback: A takes 2000 (legacy path, legacy key)
	if api.refunded["pi_A"] != 2000 || api.idemKeys[0] != "ut-refund-evt-1-pi_A" {
		t.Fatalf("refunded %v keys %q", api.refunded, api.idemKeys)
	}
}

func TestLegsSingleLegPartial(t *testing.T) {
	api := newLedger(map[string]int64{"pi_A": 3000})
	p := legPay(1500, "evt-1", leg("pi_A", 3000, i64(1500)))
	id, d := refundSale(api, memKV{}, noLinks(), p)
	if d != "" || id == "" || api.refunded["pi_A"] != 1500 || len(api.idemKeys) != 1 {
		t.Fatalf("%q %q %v", id, d, api.refunded)
	}
}

func TestLegsTwoLegsEachOwnAmount(t *testing.T) {
	api := newLedger(map[string]int64{"pi_A": 3000, "pi_B": 2000})
	p := legPay(5000, "evt-1", leg("pi_A", 3000, i64(3000)), leg("pi_B", 2000, i64(2000)))
	id, d := refundSale(api, memKV{}, noLinks(), p)
	if d != "" || id != "re_1" {
		t.Fatalf("id %q decline %q", id, d)
	}
	if api.refunded["pi_A"] != 3000 || api.refunded["pi_B"] != 2000 {
		t.Fatalf("refunded %v", api.refunded)
	}
	want := []string{"ut-refund-evt-1-pi_A-3000", "ut-refund-evt-1-pi_B-2000"}
	if strings.Join(api.idemKeys, ",") != strings.Join(want, ",") {
		t.Fatalf("keys %q", api.idemKeys)
	}
}

func TestLegsSecondFailsThenRetry(t *testing.T) {
	api := newLedger(map[string]int64{"pi_A": 3000, "pi_B": 1000}) // B too small
	p := legPay(5000, "evt-1", leg("pi_A", 3000, i64(3000)), leg("pi_B", 2000, i64(2000)))
	kv := memKV{}
	if _, d := refundSale(api, kv, noLinks(), p); d != declineRefundPartial {
		t.Fatalf("want refund_partial, got %q", d)
	}
	// Stripe stores the failed answer under B's key (documented): a retry of
	// the same form replays it, and A is replayed, never refunded twice.
	if _, d := refundSale(api, kv, noLinks(), p); d != declineRefundPartial || api.refunded["pi_A"] != 3000 {
		t.Fatalf("retry: %q %v", d, api.refunded)
	}
	// a different event id is a new refund: leg A has nothing left
	p2 := p
	p2.EventID = "evt-2"
	if _, d := refundSale(api, kv, noLinks(), p2); d == "" {
		t.Fatal("new event must not be replayed")
	}
}

func TestLegsSecondLostReplyThenRetry(t *testing.T) {
	api := newLedger(map[string]int64{"pi_A": 3000, "pi_B": 2000})
	p := legPay(5000, "evt-1", leg("pi_A", 3000, i64(3000)), leg("pi_B", 2000, i64(2000)))
	// reply of the 2nd call lost: emulate by losing replies after the first call
	api2 := &afterN{ledgerStripe: api, loseAfter: 1}
	if _, d := refundSale(api2, memKV{}, noLinks(), p); d != declineRefundPartial {
		t.Fatalf("got %q", d)
	}
	if _, d := refundSale(api2, memKV{}, noLinks(), p); d != "" {
		t.Fatalf("retry %q", d)
	}
	if api.refunded["pi_A"] != 3000 || api.refunded["pi_B"] != 2000 {
		t.Fatalf("refunded %v", api.refunded)
	}
}

// afterN loses the reply of exactly the 2nd call.
type afterN struct {
	*ledgerStripe
	loseAfter int
	calls     int
}

func (a *afterN) call(m, p, f, k string) ([]byte, int, bool) {
	a.calls++
	if a.calls == a.loseAfter+1 {
		a.ledgerStripe.loseReplies = 1
	}
	return a.ledgerStripe.call(m, p, f, k)
}

func TestLegsFirstLegFailsIsNotPartial(t *testing.T) {
	api := newLedger(map[string]int64{"pi_A": 100, "pi_B": 2000})
	p := legPay(5000, "evt-1", leg("pi_A", 3000, i64(3000)), leg("pi_B", 2000, i64(2000)))
	if _, d := refundSale(api, memKV{}, noLinks(), p); d != "amount_too_large" {
		t.Fatalf("got %q", d)
	}
	if len(api.idemKeys) != 1 {
		t.Fatalf("must stop at first failure: %q", api.idemKeys)
	}
}

func TestLegsInvalidNoCall(t *testing.T) {
	many := []origPayment{}
	for i := 0; i < 17; i++ {
		many = append(many, leg("pi_"+strings.Repeat("x", i+1), 100, i64(100)))
	}
	cases := []struct {
		name string
		p    eventPayload
		want string
	}{
		{"zero", legPay(0, "e", leg("pi_A", 100, i64(0)), leg("pi_B", 100, i64(0))), declineInvalidRefundAmount},
		{"negative", legPay(100, "e", leg("pi_A", 300, i64(-100)), leg("pi_B", 100, i64(200))), declineInvalidRefundAmount},
		{"over leg", legPay(300, "e", leg("pi_A", 100, i64(300))), declineInvalidRefundAmount},
		{"sum mismatch", legPay(500, "e", leg("pi_A", 300, i64(100)), leg("pi_B", 300, i64(100))), declineInvalidRefundAmount},
		{"overflow", legPay(10, "e", leg("pi_A", 1<<62, i64(1<<62)), leg("pi_B", 1<<62, i64(1<<62)), leg("pi_C", 1<<62, i64(1<<62))), declineInvalidRefundAmount},
		{"missing ref", legPay(200, "e", leg("", 100, i64(100)), leg("pi_B", 100, i64(100))), declineUnknownOriginal},
		{"one leg without field", legPay(200, "e", leg("pi_A", 100, i64(100)), leg("pi_B", 100, nil)), declineInvalidRefundAmount},
		{"negative amount", legPay(100, "e", origPayment{TransactionRef: "pi_A", Amount: -5, RefundAmount: i64(100)}), declineInvalidRefundAmount},
		{"17 legs", legPay(1700, "e", many...), declineTooManyLegs},
	}
	for _, c := range cases {
		api := newLedger(map[string]int64{})
		if _, d := refundSale(api, memKV{}, noLinks(), c.p); d != c.want || len(api.idemKeys) != 0 {
			t.Errorf("%s: got %q calls %d, want %q", c.name, d, len(api.idemKeys), c.want)
		}
	}
}

func TestLegsUpgradeFromV170KeyNoClash(t *testing.T) {
	api := newLedger(map[string]int64{"pi_A": 3000, "pi_B": 2000})
	// v1.7.0 sent the whole amount on leg A with the legacy key and got amount_too_large.
	legacy := legPay(5000, "evt-1", leg("pi_A", 3000, nil), leg("pi_B", 2000, nil))
	if _, d := refundSale(api, memKV{}, noLinks(), legacy); d == "" {
		t.Fatal("legacy must fail on 5000 over 3000/2000")
	}
	p := legPay(5000, "evt-1", leg("pi_A", 3000, i64(3000)), leg("pi_B", 2000, i64(2000)))
	if _, d := refundSale(api, memKV{}, noLinks(), p); d != "" {
		t.Fatalf("per-leg retry after upgrade: %q", d)
	}
}

func TestLegsNoEventIDNoKey(t *testing.T) {
	api := newLedger(map[string]int64{"pi_A": 3000})
	p := legPay(1500, "", leg("pi_A", 3000, i64(1500)))
	if _, d := refundSale(api, memKV{}, noLinks(), p); d != "" || api.idemKeys[0] != "" {
		t.Fatalf("%q %q", d, api.idemKeys)
	}
}

func TestLegsReversalIgnoresRefundAmount(t *testing.T) {
	api := newLedger(map[string]int64{"pi_A": 3000})
	p := legPay(900, "evt-1", leg("pi_A", 3000, i64(100)))
	p.Reversal = true
	if _, d := refundSale(api, memKV{}, noLinks(), p); d != "" {
		t.Fatal(d)
	}
	if api.refunded["pi_A"] != 900 || api.idemKeys[0] != "ut-refund-evt-1-pi_A" {
		t.Fatalf("%v %q", api.refunded, api.idemKeys)
	}
}

func TestLegsParsesRefundAmountPresenceAware(t *testing.T) {
	var p eventPayload
	_ = json.Unmarshal([]byte(`{"original_payments":[{"transaction_ref":"a","refund_amount":0},{"transaction_ref":"b"}]}`), &p)
	if p.OriginalPayments[0].RefundAmount == nil || *p.OriginalPayments[0].RefundAmount != 0 || p.OriginalPayments[1].RefundAmount != nil {
		t.Fatalf("%+v", p.OriginalPayments)
	}
}

func TestLegsLogsMasked(t *testing.T) {
	logs := captureLogs(t)
	api := newLedger(map[string]int64{"pi_AAAAAAAAAA1234": 3000, "pi_BBBBBBBBBB5678": 100})
	p := legPay(5000, "evt-1", leg("pi_AAAAAAAAAA1234", 3000, i64(3000)), leg("pi_BBBBBBBBBB5678", 2000, i64(2000)))
	refundSale(api, memKV{}, noLinks(), p)
	if len(*logs) == 0 {
		t.Fatal("expected a log line")
	}
	for _, l := range *logs {
		if strings.Contains(l, "pi_AAAA") || strings.Contains(l, "pi_BBBB") || strings.Contains(l, "re_1") && strings.Contains(l, "re_1 ") {
			t.Errorf("unmasked log: %s", l)
		}
	}
	if !strings.Contains(strings.Join(*logs, "\n"), "5678") {
		t.Errorf("failed leg not named masked: %q", *logs)
	}
}
