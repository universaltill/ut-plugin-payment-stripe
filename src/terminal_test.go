package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// hostGate mirrors universal-till's paymentGateTimeout (internal/plugins/
// wasm_runtime.go): the host kills the guest at 30 s, so every reader flow
// must have finished — cancel included — before then (ut-docs#6304).
const hostGate = 30 * time.Second

// fakeClock advances only when the flow sleeps or makes a call.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time        { return c.t }
func (c *fakeClock) sleep(d time.Duration) { c.t = c.t.Add(d) }

type memKV map[string]string

func (m memKV) get(k string) string    { return m[k] }
func (m memKV) put(k string, v []byte) { m[k] = string(v) }

// fakeStripe simulates one Stripe account with one Terminal reader. The
// customer taps at tapAt (relative to the flow's start); the payment is
// then "processing" for procDur and "succeeded" after. A tap is only
// possible while the reader action is live: cancel_action ends it, and a
// cancelled PaymentIntent can never be charged.
type fakeStripe struct {
	clk     *fakeClock
	start   time.Time
	latency time.Duration
	tapAt   time.Duration // 0 = never taps
	procDur time.Duration

	pis          map[string]*fakePI
	order        []string
	creates      int
	idemKeys     []string
	cancelAction int
	piCancels    int
	processFails bool
}

type fakePI struct {
	status   string // requires_payment_method, processing, succeeded, canceled
	onReader bool
	tappedAt time.Time
}

func newFakeStripe(clk *fakeClock) *fakeStripe {
	return &fakeStripe{clk: clk, start: clk.t, latency: 300 * time.Millisecond, procDur: 2 * time.Second, pis: map[string]*fakePI{}}
}

// advance applies the customer's tap and the processing time to every PI.
func (f *fakeStripe) advance() {
	for _, id := range f.order {
		p := f.pis[id]
		if p.status == "requires_payment_method" && p.onReader && f.tapAt > 0 && !f.clk.t.Before(f.start.Add(f.tapAt)) {
			p.status, p.tappedAt = "processing", f.start.Add(f.tapAt)
		}
		if p.status == "processing" && !f.clk.t.Before(p.tappedAt.Add(f.procDur)) {
			p.status, p.onReader = "succeeded", false
		}
	}
}

func (f *fakeStripe) charged() int {
	f.advance()
	n := 0
	for _, p := range f.pis {
		if p.status == "succeeded" {
			n++
		}
	}
	return n
}

func (f *fakeStripe) call(method, path, form, idemKey string) ([]byte, bool) {
	f.clk.sleep(f.latency)
	f.advance()
	errBody := func(code string) []byte {
		b, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code}})
		return b
	}
	piBody := func(id string) []byte {
		b, _ := json.Marshal(map[string]string{"id": id, "status": f.pis[id].status})
		return b
	}
	switch {
	case method == "POST" && path == "/v1/payment_intents":
		if idemKey != "" {
			f.idemKeys = append(f.idemKeys, idemKey)
		}
		f.creates++
		id := "pi_" + string(rune('a'+len(f.order)))
		f.pis[id] = &fakePI{status: "requires_payment_method"}
		f.order = append(f.order, id)
		return piBody(id), true
	case method == "POST" && strings.HasSuffix(path, "/process_payment_intent"):
		if f.processFails {
			return errBody("terminal_reader_offline"), true
		}
		id := strings.TrimPrefix(form, "payment_intent=")
		f.pis[id].onReader = true
		return []byte(`{"id":"tmr_1"}`), true
	case method == "POST" && strings.Contains(path, "/test_helpers/"):
		return []byte(`{}`), true
	case method == "POST" && strings.HasSuffix(path, "/cancel_action"):
		f.cancelAction++
		for _, p := range f.pis {
			if p.status == "processing" {
				return errBody("terminal_reader_busy"), true
			}
		}
		for _, p := range f.pis {
			p.onReader = false
		}
		return []byte(`{"id":"tmr_1","action":null}`), true
	case method == "POST" && strings.HasSuffix(path, "/cancel"):
		f.piCancels++
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/payment_intents/"), "/cancel")
		p := f.pis[id]
		if p.status == "processing" || p.status == "succeeded" {
			return errBody("payment_intent_unexpected_state"), true
		}
		p.status, p.onReader = "canceled", false
		return piBody(id), true
	case method == "GET" && strings.HasPrefix(path, "/v1/payment_intents/"):
		return piBody(strings.TrimPrefix(path, "/v1/payment_intents/")), true
	}
	return errBody("unexpected_call"), true
}

func newCharger(f *fakeStripe, kv memKV, requestID string) *terminalCharger {
	return &terminalCharger{
		api: f, clk: f.clk, kv: kv,
		sk: "sk_live_x", currency: "gbp", readerID: "tmr_1",
		requestID: requestID, amount: 1250, start: f.clk.t,
	}
}

func run(t *testing.T, c *terminalCharger, clk *fakeClock) outcome {
	t.Helper()
	start := clk.t
	out := c.run()
	if took := clk.t.Sub(start); took >= hostGate {
		t.Fatalf("flow took %s, past the host's %s payment gate: the till has already shown the sale unpaid and killed the plugin", took, hostGate)
	}
	return out
}

// A customer who taps after the till gave up must not be charged: the
// reader action is cancelled inside the gate (ut-docs#6304).
func TestLateTapAfterTimeoutIsCancelledNotCharged(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeStripe(clk)
	f.tapAt = 29 * time.Second
	out := run(t, newCharger(f, memKV{}, "att:0:ab"), clk)
	if out.approved {
		t.Fatalf("approved with no tap inside the window: %+v", out)
	}
	if f.cancelAction == 0 || f.piCancels == 0 {
		t.Fatalf("reader action / PaymentIntent not cancelled (cancel_action=%d, pi cancel=%d)", f.cancelAction, f.piCancels)
	}
	clk.sleep(60 * time.Second) // the customer taps afterwards
	if n := f.charged(); n != 0 {
		t.Fatalf("customer charged %d time(s) for a sale the till showed unpaid", n)
	}
}

// A tap that lands while we cancel cannot be stopped; the charge must then
// be reported as paid, never dropped.
func TestTapDuringCancelIsReportedPaid(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeStripe(clk)
	f.tapAt = 17900 * time.Millisecond
	f.procDur = 3 * time.Second
	out := run(t, newCharger(f, memKV{}, "att:0:ab"), clk)
	if !out.approved || out.authCode == "" {
		t.Fatalf("captured payment not surfaced as approved: %+v", out)
	}
}

// Processing that never settles before the gate: decline outcome_unknown
// (not a plain timeout), and remember the PaymentIntent for the retry.
func TestUnsettledAtDeadlineIsOutcomeUnknown(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeStripe(clk)
	f.tapAt = 17 * time.Second
	f.procDur = time.Minute
	kv := memKV{}
	out := run(t, newCharger(f, kv, "att:0:ab"), clk)
	if out.approved || out.reason != "outcome_unknown" {
		t.Fatalf("want declined outcome_unknown, got %+v", out)
	}
	if !strings.Contains(kv["attempt:att:0:ab"], "pi_a") {
		t.Fatalf("attempt record not kept: %q", kv["attempt:att:0:ab"])
	}
}

// The cashier re-taps Pay on the same basket (same request id) after the
// late payment went through: approve that payment, never charge twice.
func TestRetryAfterLateSuccessApprovesWithoutSecondCharge(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeStripe(clk)
	f.tapAt = 17 * time.Second
	f.procDur = 15 * time.Second
	kv := memKV{}
	first := run(t, newCharger(f, kv, "att:0:ab"), clk)
	if first.approved {
		t.Fatalf("first attempt should not know the outcome yet: %+v", first)
	}
	clk.sleep(30 * time.Second)
	f.start = clk.t
	f.tapAt = 0
	second := run(t, newCharger(f, kv, "att:0:ab"), clk)
	if !second.approved || second.authCode != "pi_a" {
		t.Fatalf("retry did not surface the late payment: %+v", second)
	}
	if f.creates != 1 || f.charged() != 1 {
		t.Fatalf("want 1 PaymentIntent and 1 charge, got %d / %d", f.creates, f.charged())
	}
}

// A retry after a cancelled attempt starts a fresh PaymentIntent under a
// new idempotency key (the old key would replay the cancelled one).
func TestRetryAfterCancelledAttemptChargesFresh(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeStripe(clk)
	kv := memKV{}
	first := run(t, newCharger(f, kv, "att:0:ab"), clk)
	if first.approved {
		t.Fatalf("no tap but approved: %+v", first)
	}
	clk.sleep(5 * time.Second)
	f.start = clk.t
	f.tapAt = 3 * time.Second
	second := run(t, newCharger(f, kv, "att:0:ab"), clk)
	if !second.approved || second.authCode != "pi_b" {
		t.Fatalf("retry should charge a fresh PI: %+v", second)
	}
	if len(f.idemKeys) != 2 || f.idemKeys[0] == f.idemKeys[1] {
		t.Fatalf("idempotency keys must differ per attempt: %v", f.idemKeys)
	}
}

// A reader that refuses the payment leaves a PaymentIntent behind; cancel
// it so nothing can collect it later.
func TestReaderRefusalCancelsPaymentIntent(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeStripe(clk)
	f.processFails = true
	out := run(t, newCharger(f, memKV{}, "att:0:ab"), clk)
	if out.approved || out.reason != "terminal_reader_offline" {
		t.Fatalf("want declined terminal_reader_offline, got %+v", out)
	}
	if f.piCancels == 0 {
		t.Fatal("orphan PaymentIntent was not cancelled")
	}
}

// A normal tap inside the window approves as before.
func TestPromptTapApproves(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeStripe(clk)
	f.tapAt = 4 * time.Second
	out := run(t, newCharger(f, memKV{}, "att:0:ab"), clk)
	if !out.approved || out.authCode != "pi_a" || f.cancelAction != 0 {
		t.Fatalf("prompt tap: %+v cancel_action=%d", out, f.cancelAction)
	}
}
