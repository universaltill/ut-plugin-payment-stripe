package main

import (
	"strings"
	"testing"
)

// Fixtures follow Stripe's DOCUMENTED shapes (payout object, balance
// transaction list). They are not recordings; the identifier proof against
// test mode waits for the owner key (ut-docs#6579, #7081).

const payoutsPage = `{"object":"list","has_more":false,"data":[
 {"id":"po_auto1","object":"payout","amount":4750,"currency":"gbp","status":"paid","automatic":true,"method":"standard","arrival_date":1764028800},
 {"id":"po_inst1","object":"payout","amount":900,"currency":"gbp","status":"paid","automatic":false,"method":"instant","arrival_date":1764028800}
]}`

// charge 2000 fee 80, charge 3000 fee 120, refund -500: lines net 4300.
const balancePage = `{"object":"list","has_more":false,"data":[
 {"id":"txn_1","amount":2000,"fee":80,"net":1920,"currency":"gbp","type":"charge","created":1763900000,"available_on":1764000000,"source":{"id":"ch_1","object":"charge","payment_intent":"pi_1"}},
 {"id":"txn_2","amount":3000,"fee":120,"net":2880,"currency":"gbp","type":"payment","created":1763900100,"available_on":1764000000,"source":"ch_2"},
 {"id":"txn_3","amount":-500,"fee":0,"net":-500,"currency":"gbp","type":"refund","created":1763900200,"available_on":1764000000,"source":{"id":"re_1","object":"refund","payment_intent":"pi_1"}}
]}`

func TestParsePayoutsSplitsAutomaticFromInstant(t *testing.T) {
	ps, more, last, err := parsePayouts([]byte(payoutsPage))
	if err != nil || more || last != "po_inst1" || len(ps) != 2 {
		t.Fatalf("ps=%v more=%v last=%q err=%v", ps, more, last, err)
	}
	if !ps[0].Automatic || ps[0].ID != "po_auto1" || ps[0].Amount != 4750 || ps[0].Status != "paid" {
		t.Fatalf("auto payout: %+v", ps[0])
	}
	if ps[1].Automatic {
		t.Fatalf("instant payout must not be automatic: %+v", ps[1])
	}
}

func TestParseBalanceTxnsSignedMinorUnitsAndRefs(t *testing.T) {
	ls, more, last, err := parseBalanceTxns([]byte(balancePage), "po_auto1")
	if err != nil || more || last != "txn_3" || len(ls) != 3 {
		t.Fatalf("ls=%v more=%v last=%q err=%v", ls, more, last, err)
	}
	a := ls[0]
	if a.TransactionRef != "pi_1" || a.Kind != "sale" || a.Gross != 2000 || a.Fee != 80 || a.Net != 1920 ||
		a.Currency != "GBP" || a.PayoutRef != "po_auto1" || a.OccurredAt != "2025-11-23T12:13:20Z" || a.SettledAt != "2025-11-24T16:00:00Z" {
		t.Fatalf("sale: %+v", a)
	}
	// Unexpanded source is a charge id: kept, but flagged, never guessed.
	if b := ls[1]; b.TransactionRef != "ch_2" || b.RefKind != "charge" {
		t.Fatalf("charge-only: %+v", b)
	}
	if r := ls[2]; r.Kind != "refund" || r.Gross != -500 || r.Net != -500 || r.TransactionRef != "pi_1" {
		t.Fatalf("refund: %+v", r)
	}
}

func TestParseBalanceTxnsRejectsDecimalAmounts(t *testing.T) {
	for _, amt := range []string{"19.99", "0.01", `"12"`} {
		body := `{"data":[{"id":"txn_x","amount":` + amt + `,"fee":0,"net":0,"currency":"gbp","type":"charge","source":"ch_x"}]}`
		if _, _, _, err := parseBalanceTxns([]byte(body), "po_1"); err == nil {
			t.Errorf("amount %s must be rejected, never rounded", amt)
		}
	}
}

func TestParseBalanceTxnsMapsKindsAndSkipsPayoutRow(t *testing.T) {
	body := `{"object":"list","has_more":true,"data":[
	 {"id":"txn_t1","amount":-1500,"fee":0,"net":-1500,"currency":"gbp","type":"dispute","source":"dp_1"},
	 {"id":"txn_t2","amount":-15,"fee":0,"net":-15,"currency":"gbp","type":"stripe_fee","source":"x"},
	 {"id":"txn_t3","amount":-4300,"fee":0,"net":-4300,"currency":"gbp","type":"payout","source":"po_auto1"}]}`
	ls, more, last, err := parseBalanceTxns([]byte(body), "po_auto1")
	if err != nil || !more || last != "txn_t3" || len(ls) != 2 {
		t.Fatalf("ls=%v more=%v last=%q err=%v", ls, more, last, err)
	}
	if ls[0].Kind != "chargeback" || ls[1].Kind != "adjustment" {
		t.Fatalf("kinds: %+v", ls)
	}
}

func TestBuildReconcileLinesSumToPayoutNetAndInstantIsCannotMatch(t *testing.T) {
	ps, _, _, _ := parsePayouts([]byte(payoutsPage))
	ls, _, _, _ := parseBalanceTxns([]byte(balancePage), "po_auto1")
	// 1920 + 2880 - 500 = 4300; the payout says 4750, so it must NOT reconcile.
	res := buildReconcile(ps, map[string][]settledLine{"po_auto1": ls})
	if len(res.Lines) != 3 || len(res.Payouts) != 1 || res.Payouts[0].Balanced {
		t.Fatalf("unbalanced payout must be flagged: %+v", res.Payouts)
	}
	if res.Payouts[0].LinesNet != 4300 {
		t.Fatalf("lines net: %+v", res.Payouts[0])
	}
	if len(res.CannotMatch) != 1 || res.CannotMatch[0].PayoutRef != "po_inst1" || res.CannotMatch[0].Reason != "payout_not_automatic" {
		t.Fatalf("instant payout silently dropped: %+v", res.CannotMatch)
	}
	// A payout whose lines do sum to its amount is balanced.
	ps[0].Amount = 4300
	res = buildReconcile(ps, map[string][]settledLine{"po_auto1": ls})
	if !res.Payouts[0].Balanced {
		t.Fatalf("expected balanced: %+v", res.Payouts)
	}
}

func TestBuildReconcileAutomaticPayoutWithNoLinesIsCannotMatch(t *testing.T) {
	ps, _, _, _ := parsePayouts([]byte(payoutsPage))
	res := buildReconcile(ps, nil)
	found := false
	for _, c := range res.CannotMatch {
		if c.PayoutRef == "po_auto1" && c.Reason == "no_balance_transactions" {
			found = true
		}
	}
	if !found {
		t.Fatalf("%+v", res.CannotMatch)
	}
}

func TestParseReconcileMalformed(t *testing.T) {
	if _, _, _, err := parsePayouts([]byte(`not json`)); err == nil {
		t.Error("payouts: want error")
	}
	if _, _, _, err := parseBalanceTxns([]byte(`{"data":"x"}`), "po_1"); err == nil {
		t.Error("balance: want error")
	}
	// Errors never echo the body (it can carry the key in an error message).
	_, _, _, err := parsePayouts([]byte(`{"error":{"message":"Invalid API Key sk_live_SECRET"}}x`))
	if err != nil && strings.Contains(err.Error(), "sk_live") {
		t.Errorf("error leaks body: %v", err)
	}
}

// Reviewer (payments-batch): fail closed on Stripe error bodies and statuses.
func TestParseReconcileStripeErrorBodyIsAnErrorNotEmpty(t *testing.T) {
	bad := []byte(`{"error":{"type":"invalid_request_error","message":"nope"}}`)
	if _, _, _, err := parsePayouts(bad); err == nil {
		t.Error("payouts: a Stripe error body must be an error, not an empty page")
	}
	if _, _, _, err := parseBalanceTxns(bad, "po_1"); err == nil {
		t.Error("balance: a Stripe error body must be an error, not an empty page")
	}
	if _, _, _, err := parsePayouts([]byte(`{"data":[]}`)); err == nil {
		t.Error("payouts: not a list object must be an error")
	}
}

func TestParseBalanceTxnsRefusesUnmatchableOrInconsistentRows(t *testing.T) {
	noSrc := `{"object":"list","has_more":false,"data":[{"id":"txn_1","amount":100,"fee":0,"net":100,"currency":"gbp","type":"charge","created":1,"available_on":2}]}`
	if _, _, _, err := parseBalanceTxns([]byte(noSrc), "po_1"); err == nil {
		t.Error("a charge with no source id must be refused")
	}
	badNet := `{"object":"list","has_more":false,"data":[{"id":"txn_1","amount":100,"fee":10,"net":100,"currency":"gbp","type":"charge","created":1,"available_on":2,"source":"ch_1"}]}`
	if _, _, _, err := parseBalanceTxns([]byte(badNet), "po_1"); err == nil {
		t.Error("net != amount - fee must be refused")
	}
}

func TestBuildReconcileUnknownOrFailedPayoutStatusIsCannotMatch(t *testing.T) {
	line := settledLine{Net: 100, Currency: "GBP"}
	by := map[string][]settledLine{"po_a": {line}, "po_b": {line}, "po_c": {line}, "po_d": {line}}
	res := buildReconcile([]stripePayout{
		{ID: "po_a", Amount: 100, Currency: "GBP", Status: "failed", Automatic: true},
		{ID: "po_b", Amount: 100, Currency: "GBP", Status: "", Automatic: true},
		{ID: "po_c", Amount: 100, Currency: "GBP", Status: "in_transit", Automatic: true},
		{ID: "po_d", Amount: 100, Currency: "USD", Status: "paid", Automatic: true},
	}, by)
	if len(res.CannotMatch) != 2 || res.CannotMatch[0].Reason != "payout_failed" || res.CannotMatch[1].Reason != "unknown_payout_status" {
		t.Fatalf("cannot_match %+v", res.CannotMatch)
	}
	if len(res.Payouts) != 2 || res.Payouts[0].PayoutState != "pending" || res.Payouts[1].Balanced {
		t.Fatalf("payouts %+v", res.Payouts)
	}
}
