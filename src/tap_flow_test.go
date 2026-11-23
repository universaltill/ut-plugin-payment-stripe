package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// fakeBroker records every host call in order and answers from scripts.
type fakeBroker struct {
	calls       []string // "tap", "status", "cancel", "refund"
	tapRC       int32
	tapAnswer   string
	statusAns   []string // session_status answers in order (last repeats)
	statusRC    int32
	cancelAns   string
	refundAns   []string
	refundRC    int32
	kv          *orderKV
	recordAtTap string
	refundAmts  []int64
	statusN     int
}

func (b *fakeBroker) tapRequest() ([]byte, int32) {
	if b.kv != nil {
		b.recordAtTap = b.kv.get(tapAttemptsKey)
	}
	b.calls = append(b.calls, "tap")
	if b.tapRC < 0 {
		return nil, b.tapRC
	}
	return []byte(b.tapAnswer), int32(len(b.tapAnswer))
}

func (b *fakeBroker) providerCall(provider, op string, body []byte) ([]byte, int32) {
	if provider != "stripe" {
		panic("provider must be stripe, got " + provider)
	}
	switch op {
	case "session_status":
		b.calls = append(b.calls, "status")
		if b.statusRC < 0 {
			return nil, b.statusRC
		}
		i := b.statusN
		b.statusN++
		if i >= len(b.statusAns) {
			i = len(b.statusAns) - 1
		}
		a := b.statusAns[i]
		return []byte(a), int32(len(a))
	case "session_cancel":
		b.calls = append(b.calls, "cancel")
		return []byte(b.cancelAns), int32(len(b.cancelAns))
	case "refund":
		var req struct {
			Amount int64 `json:"amount_minor"`
		}
		_ = json.Unmarshal(body, &req)
		b.refundAmts = append(b.refundAmts, req.Amount)
		b.calls = append(b.calls, "refund")
		if b.refundRC < 0 {
			return nil, b.refundRC
		}
		i := len(b.refundAmts) - 1
		if i >= len(b.refundAns) {
			i = len(b.refundAns) - 1
		}
		return []byte(b.refundAns[i]), int32(len(b.refundAns[i]))
	}
	panic("unexpected op " + op)
}

type orderKV struct{ memKV }

func newBroker() (*fakeBroker, *orderKV) {
	kv := &orderKV{memKV{}}
	return &fakeBroker{kv: kv}, kv
}

const approvedTap = `{"status":"approved","provider_txn_id":"pi_test1","card_brand":"visa","last4":"4242"}`

func authEv(key string) tapRequest {
	return tapRequest{Key: key, Amount: 1000, Currency: "gbp"}
}

func countCalls(b *fakeBroker, name string) int {
	n := 0
	for _, c := range b.calls {
		if c == name {
			n++
		}
	}
	return n
}

func TestTapAuthorize_FreshApproved(t *testing.T) {
	b, kv := newBroker()
	b.tapAnswer = approvedTap
	out := tapAuthorize(b, kv, authEv("att:0:abc"), 1700000000)
	if out.kind != tapApproved || out.ref != "pi_test1" {
		t.Fatalf("want approved pi_test1, got %+v", out)
	}
	if !strings.Contains(b.recordAtTap, `"pending"`) || !strings.Contains(b.recordAtTap, "att:0:abc") {
		t.Fatalf("pending record must exist BEFORE the tap call, got %q", b.recordAtTap)
	}
	rec, ok := findTapAttempt(kv, "att:0:abc")
	if !ok || rec.State != "approved" || rec.PI != "pi_test1" || rec.Amount != 1000 || rec.Currency != "gbp" {
		t.Fatalf("record after approve = %+v", rec)
	}
}

func TestTapAuthorize_Declined_10_05(t *testing.T) {
	b, kv := newBroker()
	b.tapAnswer = `{"status":"declined","decline_code":"card_declined"}`
	out := tapAuthorize(b, kv, authEv("k1"), 1)
	if out.kind != tapDeclined || out.code != "card_declined" {
		t.Fatalf("got %+v", out)
	}
	if rec, _ := findTapAttempt(kv, "k1"); rec.State != "declined" {
		t.Fatalf("record state %q", rec.State)
	}
	// A later authorize with the same key taps again (declined = nothing charged).
	b.tapAnswer = approvedTap
	out = tapAuthorize(b, kv, authEv("k1"), 2)
	if out.kind != tapApproved {
		t.Fatalf("second try %+v", out)
	}
	if n := countCalls(b, "tap"); n != 2 {
		t.Fatalf("taps = %d, want 2", n)
	}
}

func TestTapAuthorize_PIN_10_03_OfflinePin(t *testing.T) {
	b, kv := newBroker()
	b.tapAnswer = `{"status":"declined","decline_code":"offline_pin_required"}`
	out := tapAuthorize(b, kv, authEv("k"), 1)
	if out.kind != tapDeclined || out.code != "offline_pin_required" {
		t.Fatalf("got %+v", out)
	}
}

func TestTapAuthorize_RetryAfterSucceeded(t *testing.T) {
	b, kv := newBroker()
	putTapAttempt(kv, tapAttempt{ID: "k", State: "pending", Amount: 1000, Currency: "gbp", At: 1})
	b.statusAns = []string{`{"id":"pi_test9","status":"succeeded","amount_minor":1000,"amount_received_minor":1000,"currency":"gbp"}`}
	out := tapAuthorize(b, kv, authEv("k"), 5)
	if out.kind != tapApproved || out.ref != "pi_test9" {
		t.Fatalf("got %+v", out)
	}
	if n := countCalls(b, "tap"); n != 0 {
		t.Fatalf("tap_to_pay_request calls = %d, want 0", n)
	}
}

func TestTapAuthorize_RetrySucceededWrongAmountIsPending(t *testing.T) {
	b, kv := newBroker()
	putTapAttempt(kv, tapAttempt{ID: "k", State: "pending", Amount: 1000, Currency: "gbp"})
	b.statusAns = []string{`{"id":"pi_x","status":"succeeded","amount_received_minor":900,"currency":"gbp"}`}
	if out := tapAuthorize(b, kv, authEv("k"), 5); out.kind != tapPending {
		t.Fatalf("got %+v", out)
	}
}

func TestTapAuthorize_RetryRequiresPaymentMethod(t *testing.T) {
	b, kv := newBroker()
	putTapAttempt(kv, tapAttempt{ID: "k", State: "pending", Amount: 1000, Currency: "gbp"})
	b.statusAns = []string{`{"status":"requires_payment_method"}`}
	b.cancelAns = `{"id":"pi_a","status":"canceled"}`
	b.tapAnswer = approvedTap
	out := tapAuthorize(b, kv, authEv("k"), 5)
	if out.kind != tapApproved {
		t.Fatalf("got %+v", out)
	}
	if got := strings.Join(b.calls, ","); got != "status,cancel,tap" {
		t.Fatalf("calls = %s, want status,cancel,tap", got)
	}
}

func TestTapAuthorize_RetryCancelFailsIsPending(t *testing.T) {
	b, kv := newBroker()
	putTapAttempt(kv, tapAttempt{ID: "k", State: "pending", Amount: 1000, Currency: "gbp"})
	b.statusAns = []string{`{"status":"requires_payment_method"}`}
	b.cancelAns = `{"error":{"code":"provider_unavailable"}}`
	if out := tapAuthorize(b, kv, authEv("k"), 5); out.kind != tapPending {
		t.Fatalf("got %+v", out)
	}
	if countCalls(b, "tap") != 0 {
		t.Fatal("must not tap after a failed cancel")
	}
}

func TestTapAuthorize_RetryUnsettledStatesArePending(t *testing.T) {
	for name, ans := range map[string]string{
		"processing":       `{"status":"processing"}`,
		"requires_capture": `{"status":"requires_capture"}`,
		"unknown":          `{"status":"unknown","retry_after_s":30}`,
		"lookup_error":     `{"error":{"code":"provider_unavailable"}}`,
		"not_found":        `{"error":{"code":"payment_not_found"}}`,
		"garbage":          `not json`,
	} {
		b, kv := newBroker()
		putTapAttempt(kv, tapAttempt{ID: "k", State: "pending", Amount: 1000, Currency: "gbp"})
		b.statusAns = []string{ans}
		if out := tapAuthorize(b, kv, authEv("k"), 5); out.kind != tapPending {
			t.Errorf("%s: got %+v, want pending", name, out)
		}
		if countCalls(b, "tap") != 0 {
			t.Errorf("%s: tapped again", name)
		}
	}
}

func TestTapAuthorize_RetryHostFailureIsPending(t *testing.T) {
	b, kv := newBroker()
	putTapAttempt(kv, tapAttempt{ID: "k", State: "pending", Amount: 1000, Currency: "gbp"})
	b.statusRC = -3
	if out := tapAuthorize(b, kv, authEv("k"), 5); out.kind != tapPending {
		t.Fatalf("got %+v", out)
	}
}

func TestTapAuthorize_CanceledStatusTapsAgain(t *testing.T) {
	b, kv := newBroker()
	putTapAttempt(kv, tapAttempt{ID: "k", State: "pending", Amount: 1000, Currency: "gbp"})
	b.statusAns = []string{`{"status":"canceled"}`}
	b.tapAnswer = approvedTap
	if out := tapAuthorize(b, kv, authEv("k"), 5); out.kind != tapApproved {
		t.Fatalf("got %+v", out)
	}
}

func TestTapAuthorize_TapOutcomesThatArePending(t *testing.T) {
	for name, tc := range map[string]struct {
		rc  int32
		ans string
	}{
		"processing":      {0, `{"status":"pending"}`},
		"timeout":         {0, `{"status":"pending","reason":"timeout"}`},
		"amount_mismatch": {0, `{"status":"declined","decline_code":"amount_mismatch"}`},
		"unknown_status":  {0, `{"status":"weird"}`},
		"internal":        {-3, ``},
		"bad_args":        {-4, ``},
		"unreadable":      {0, `<<`},
		"error_unknown":   {0, `{"error":{"code":"rate_limited"}}`},
		"approved_no_ref": {0, `{"status":"approved"}`},
	} {
		b, kv := newBroker()
		b.tapRC, b.tapAnswer = tc.rc, tc.ans
		if out := tapAuthorize(b, kv, authEv("k"), 5); out.kind != tapPending {
			t.Errorf("%s: got %+v, want pending (never approve, never decline)", name, out)
		}
	}
}

func TestTapAuthorize_PermissionDenied(t *testing.T) {
	b, kv := newBroker()
	b.tapRC = -2
	out := tapAuthorize(b, kv, authEv("k"), 5)
	if out.kind != tapDeclined || out.code != "start_failed" {
		t.Fatalf("got %+v", out)
	}
}

func TestTapAuthorize_NotOnboarded(t *testing.T) {
	for _, code := range []string{"not_onboarded", "not_configured", "disabled", "provider_unavailable", "invalid_amount", "currency_mismatch", "currency_unsupported", "device_role_forbidden", "device_role_unrecorded", "insecure_endpoint"} {
		b, kv := newBroker()
		b.tapAnswer = `{"error":{"code":"` + code + `"}}`
		out := tapAuthorize(b, kv, authEv("k"), 5)
		if out.kind != tapDeclined || out.code != "start_failed" {
			t.Errorf("%s: got %+v", code, out)
		}
	}
}

func TestTapAuthorize_CancelledByCustomer(t *testing.T) {
	b, kv := newBroker()
	b.tapAnswer = `{"status":"cancelled"}`
	out := tapAuthorize(b, kv, authEv("k"), 5)
	if out.kind != tapDeclined || out.code != "cancelled" {
		t.Fatalf("got %+v", out)
	}
}

func TestTapAuthorize_RecordWriteFailureStartsNothing(t *testing.T) {
	b, _ := newBroker()
	out := tapAuthorize(b, failKV{}, authEv("k"), 5)
	if out.kind != tapDeclined || out.code != "start_failed" {
		t.Fatalf("got %+v", out)
	}
	if countCalls(b, "tap") != 0 {
		t.Fatal("tapped without a record")
	}
}

type failKV struct{}

func (failKV) get(string) string        { return "" }
func (failKV) put(string, []byte) error { return os.ErrPermission }

func TestTapAuthorize_NoKeyStartsNothing(t *testing.T) {
	b, kv := newBroker()
	out := tapAuthorize(b, kv, authEv(""), 5)
	if out.kind != tapDeclined || out.code != "start_failed" || len(b.calls) != 0 {
		t.Fatalf("got %+v calls %v", out, b.calls)
	}
}

func TestTapAttempts_BoundedToEight(t *testing.T) {
	kv := memKV{}
	for i := 0; i < 20; i++ {
		putTapAttempt(kv, tapAttempt{ID: "k" + string(rune('a'+i)), State: "pending"})
	}
	var list []tapAttempt
	_ = json.Unmarshal([]byte(kv.get(tapAttemptsKey)), &list)
	if len(list) != 8 || list[7].ID != "kt" || list[0].ID != "km" {
		t.Fatalf("got %d entries first %s last %s", len(list), list[0].ID, list[len(list)-1].ID)
	}
}

func TestTapRefund_Leg(t *testing.T) {
	b, _ := newBroker()
	b.refundAns = []string{`{"refund_id":"re_1","status":"succeeded"}`, `{"refund_id":"re_2","status":"succeeded"}`}
	r6, r4 := int64(600), int64(400)
	p := eventPayload{Amount: 1000, OriginalPayments: []origPayment{
		{TransactionRef: "pi_a", Amount: 600, RefundAmount: &r6},
		{TransactionRef: "pi_b", Amount: 400, RefundAmount: &r4},
	}}
	id, reason := tapRefund(b, p)
	if reason != "" || id != "re_1" {
		t.Fatalf("id %q reason %q", id, reason)
	}
	if len(b.refundAmts) != 2 || b.refundAmts[0] != 600 || b.refundAmts[1] != 400 {
		t.Fatalf("amounts %v, want [600 400]", b.refundAmts)
	}
}

func TestTapRefund_WholeAmountWithoutLegs(t *testing.T) {
	b, _ := newBroker()
	b.refundAns = []string{`{"refund_id":"re_1","status":"succeeded"}`}
	id, reason := tapRefund(b, eventPayload{Amount: 750})
	if reason != "" || id != "re_1" || len(b.refundAmts) != 1 || b.refundAmts[0] != 750 {
		t.Fatalf("id %q reason %q amts %v", id, reason, b.refundAmts)
	}
}

func TestTapRefund_BrokerErrorFailsClosed(t *testing.T) {
	b, _ := newBroker()
	b.refundAns = []string{`{"error":{"code":"refund_exceeds"}}`}
	if id, reason := tapRefund(b, eventPayload{Amount: 750}); id != "" || reason != "refund_exceeds" {
		t.Fatalf("id %q reason %q", id, reason)
	}
	b2, _ := newBroker()
	b2.refundRC = -2
	if _, r := tapRefund(b2, eventPayload{Amount: 750}); r == "" {
		t.Fatal("denied refund must fail closed")
	}
	b3, _ := newBroker()
	b3.refundAns = []string{`{"status":"succeeded"}`} // no refund_id
	if _, r := tapRefund(b3, eventPayload{Amount: 750}); r == "" {
		t.Fatal("answer without refund_id must fail closed")
	}
	b4, _ := newBroker()
	if _, r := tapRefund(b4, eventPayload{Amount: 0}); r != declineInvalidRefundAmount || len(b4.calls) != 0 {
		t.Fatalf("zero amount: reason %q calls %v", r, b4.calls)
	}
}

func TestTapRefund_SecondLegFailsIsPartial(t *testing.T) {
	b, _ := newBroker()
	b.refundAns = []string{`{"refund_id":"re_1"}`, `{"error":{"code":"provider_unavailable"}}`}
	r6, r4 := int64(600), int64(400)
	p := eventPayload{Amount: 1000, OriginalPayments: []origPayment{
		{TransactionRef: "pi_a", Amount: 600, RefundAmount: &r6},
		{TransactionRef: "pi_b", Amount: 400, RefundAmount: &r4},
	}}
	if _, r := tapRefund(b, p); r != declineRefundPartial {
		t.Fatalf("reason %q", r)
	}
}

func TestTap_NoSecretLogged(t *testing.T) {
	var logs []string
	old := logHook
	logHook = func(m string) { logs = append(logs, m) }
	defer func() { logHook = old }()
	b, kv := newBroker()
	b.tapAnswer = `{"status":"approved","provider_txn_id":"pi_3Abc123Def456_secret_ZZZZ9","card_brand":"visa","last4":"4242"}`
	tapAuthorize(b, kv, authEv("k"), 1)
	b.refundAns = []string{`{"refund_id":"re_1","status":"succeeded","client_secret":"pi_x_secret_y","k":"rk_test_abc"}`}
	tapRefund(b, eventPayload{Amount: 10})
	all := strings.Join(logs, "\n")
	for _, bad := range []string{"_secret_", "rk_", "sk_", "pi_3Abc123Def456"} {
		if strings.Contains(all, bad) {
			t.Errorf("log leaks %q: %s", bad, all)
		}
	}
	if len(logs) == 0 {
		t.Fatal("expected some log lines")
	}
}

func TestTapGoldenHostShapes(t *testing.T) {
	// The host JSON shapes (devicepay.Result, the neutral session envelope,
	// the broker error answer) as universal-till produces them.
	var r tapResult
	if err := json.Unmarshal([]byte(`{"status":"approved","provider_txn_id":"pi_1","charge_id":"ch_1","card_brand":"visa","last4":"4242","decline_code":"","reason":""}`), &r); err != nil || r.Status != "approved" || r.ProviderTxnID != "pi_1" || r.Last4 != "4242" {
		t.Fatalf("%+v %v", r, err)
	}
	var s brokerStatus
	if err := json.Unmarshal([]byte(`{"id":"pi_1","status":"succeeded","amount_minor":1000,"amount_received_minor":1000,"currency":"gbp","card_brand":"visa","last4":"4242"}`), &s); err != nil || s.ID != "pi_1" || s.AmountReceived != 1000 {
		t.Fatalf("%+v %v", s, err)
	}
	if code := brokerErrorCode([]byte(`{"error":{"code":"not_onboarded"}}`)); code != "not_onboarded" {
		t.Fatalf("code %q", code)
	}
}

func TestTapManifest(t *testing.T) {
	raw, err := os.ReadFile("../manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Version     string   `json:"version"`
		Permissions []string `json:"permissions"`
		MinPOS      string   `json:"min_pos_version"`
		Entries     []struct {
			Type         string         `json:"type"`
			Key          string         `json:"key"`
			Label        string         `json:"label"`
			TriggerEvent string         `json:"trigger_event"`
			Config       map[string]any `json:"config"`
		} `json:"entries"`
		Hooks []struct {
			Event  string `json:"event"`
			Action string `json:"action"`
		} `json:"hooks"`
	}
	_ = json.Unmarshal(raw, &m)
	if m.Version != "1.10.0" || m.MinPOS != "0.36.3" {
		t.Fatalf("version %q min_pos %q", m.Version, m.MinPOS)
	}
	has := func(p string) bool {
		for _, x := range m.Permissions {
			if x == p {
				return true
			}
		}
		return false
	}
	if !has("device:tap_to_pay") || !has("net:payment_broker") || !has("net:api.stripe.com") {
		t.Fatalf("permissions %v", m.Permissions)
	}
	found := false
	for _, e := range m.Entries {
		if e.Key == "stripe_tap" {
			found = true
			if e.Type != "payment" || e.Config["tap"] != true || e.Config["provider"] != "stripe" || e.TriggerEvent != "payment.stripe_tap.requested" || e.Label != "payment_stripe.tap.label" {
				t.Fatalf("tap entry %+v", e)
			}
		}
		if e.Key == "stripe" && (e.Config != nil || e.TriggerEvent != "payment.stripe.requested") {
			t.Fatalf("legacy entry changed: %+v", e)
		}
	}
	if !found {
		t.Fatal("no stripe_tap entry")
	}
	want := map[string]bool{"payment.stripe_tap.authorize": false, "payment.stripe_tap.requested": false, "payment.stripe_tap.refund": false}
	for _, h := range m.Hooks {
		if _, ok := want[h.Event]; ok {
			want[h.Event] = true
		}
	}
	for ev, ok := range want {
		if !ok {
			t.Errorf("missing hook %s", ev)
		}
	}
	loc, _ := os.ReadFile("../locales/en.json")
	var l map[string]string
	_ = json.Unmarshal(loc, &l)
	if l["payment_stripe.tap.label"] != "Tap to Pay on iPhone" {
		t.Fatalf("label %q", l["payment_stripe.tap.label"])
	}
}

func TestIsTapEvent(t *testing.T) {
	if !isTapEvent("payment.stripe_tap.authorize") || isTapEvent("payment.stripe.authorize") {
		t.Fatal("isTapEvent wrong")
	}
}
