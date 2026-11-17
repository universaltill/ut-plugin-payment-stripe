package main

// Contract tests (ut-docs#6565): the Terminal flow and refund run over REAL
// HTTP, form-encoded, against a fake server that follows Stripe's DOCUMENTED
// behaviour (server-driven Terminal; idempotent requests). SCOPE: request
// paths and form bodies, Idempotency-Key semantics, reply shapes. NOT
// covered: headers/auth and the base URL (httpStripe stands in for main.go's
// wasip1-only stripeCallIdem). It is NOT a recording of Stripe: the test-mode
// Terminal key (Key Vault stripe-test-terminal-key) did not exist when this
// was written, so the simulated-reader run against real Stripe (#6440, #3055)
// is still to do. No key here is real.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type stripeWire struct {
	mu  sync.Mutex
	clk *fakeClock

	tapAt       time.Duration // 0 = never
	declineAt   time.Duration // reader declines the card at this time
	createError bool          // creates answer an api_error (replayed per idempotency key)

	pis      map[string]*wirePI
	idem     map[string]string // idempotency key -> stored response body
	start    time.Time
	reqs     []wireReq
	problems []string
	helper   int // present_payment_method calls
	refunds  []url.Values
}

type wirePI struct {
	status   string
	onReader bool
	tapped   time.Time
	declined bool
	amount   string
}

type wireReq struct {
	method, path string
	form         url.Values
	idem         string
	auth         string
}

func (s *stripeWire) fail(m string) { s.problems = append(s.problems, m) }

func (s *stripeWire) advance() {
	now := s.clk.now()
	for _, p := range s.pis {
		if p.status == "requires_payment_method" && p.onReader && s.tapAt > 0 && !now.Before(s.start.Add(s.tapAt)) {
			p.status, p.tapped = "processing", s.start.Add(s.tapAt)
		}
		if p.status == "requires_payment_method" && p.onReader && s.declineAt > 0 && !now.Before(s.start.Add(s.declineAt)) {
			p.declined = true
		}
		if p.status == "processing" && !now.Before(p.tapped.Add(2*time.Second)) {
			p.status, p.onReader = "succeeded", false
		}
	}
}

func (s *stripeWire) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(b))
	s.reqs = append(s.reqs, wireReq{r.Method, r.URL.Path, form, r.Header.Get("Idempotency-Key"), r.Header.Get("Authorization")})
	if r.Method == "POST" && r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		s.fail("Stripe takes form-encoded POST bodies, not " + r.Header.Get("Content-Type"))
	}
	s.advance()
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	errReply := func(code int, c string) {
		reply(code, map[string]any{"error": map[string]string{"code": c, "type": "invalid_request_error"}})
	}
	piJSON := func(id string) map[string]any {
		p := s.pis[id]
		m := map[string]any{"id": id, "status": p.status}
		if p.declined {
			m["last_payment_error"] = map[string]string{"code": "card_declined", "decline_code": "insufficient_funds"}
		}
		return m
	}
	path := r.URL.Path
	switch {
	case r.Method == "POST" && path == "/v1/payment_intents":
		key := r.Header.Get("Idempotency-Key")
		if prev, ok := s.idem[key]; ok && key != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(prev))
			return
		}
		if form.Get("amount") == "" || form.Get("currency") == "" ||
			form.Get("payment_method_types[]") != "card_present" || form.Get("capture_method") != "automatic" {
			s.fail("create needs amount, currency, payment_method_types[]=card_present and capture_method=automatic")
			errReply(400, "parameter_missing")
			return
		}
		var body []byte
		if s.createError {
			body, _ = json.Marshal(map[string]any{"error": map[string]string{"type": "api_error", "message": "boom"}})
		} else {
			id := "pi_" + string(rune('a'+len(s.pis)))
			s.pis[id] = &wirePI{status: "requires_payment_method", amount: form.Get("amount")}
			body, _ = json.Marshal(piJSON(id))
		}
		if key != "" {
			s.idem[key] = string(body) // Stripe replays the stored result, errors included
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	case r.Method == "POST" && strings.HasSuffix(path, "/process_payment_intent"):
		id := form.Get("payment_intent")
		if p, ok := s.pis[id]; ok {
			p.onReader = true
			reply(200, map[string]any{"id": "tmr_1", "action": map[string]any{"status": "in_progress", "type": "process_payment_intent"}})
			return
		}
		errReply(404, "resource_missing")
	case r.Method == "POST" && strings.Contains(path, "/test_helpers/terminal/readers/") && strings.HasSuffix(path, "/present_payment_method"):
		s.helper++
		reply(200, map[string]any{"id": "tmr_1"})
	case r.Method == "POST" && strings.HasSuffix(path, "/cancel_action"):
		for _, p := range s.pis {
			if p.status == "processing" {
				errReply(400, "terminal_reader_busy")
				return
			}
			p.onReader = false
		}
		reply(200, map[string]any{"id": "tmr_1", "action": nil})
	case r.Method == "POST" && strings.HasSuffix(path, "/cancel"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/payment_intents/"), "/cancel")
		p := s.pis[id]
		if p == nil {
			errReply(404, "resource_missing")
			return
		}
		if p.status == "processing" || p.status == "succeeded" {
			errReply(400, "payment_intent_unexpected_state")
			return
		}
		p.status, p.onReader = "canceled", false
		reply(200, piJSON(id))
	case r.Method == "GET" && strings.HasPrefix(path, "/v1/terminal/readers/"):
		st := "in_progress"
		for _, p := range s.pis {
			if p.declined && p.onReader {
				st = "failed"
			}
		}
		reply(200, map[string]any{"id": "tmr_1", "action": map[string]any{"status": st}})
	case r.Method == "GET" && strings.HasPrefix(path, "/v1/payment_intents/"):
		id := strings.TrimPrefix(path, "/v1/payment_intents/")
		if s.pis[id] == nil {
			errReply(404, "resource_missing")
			return
		}
		reply(200, piJSON(id))
	case r.Method == "POST" && path == "/v1/refunds":
		s.refunds = append(s.refunds, form)
		p := s.pis[form.Get("payment_intent")]
		if p == nil || p.status != "succeeded" {
			errReply(400, "charge_not_refundable")
			return
		}
		reply(200, map[string]any{"id": "re_1", "status": "succeeded"})
	default:
		errReply(404, "unexpected_call")
	}
}

type httpStripe struct{ base string }

func (h httpStripe) call(method, path, form, idemKey string) ([]byte, bool) {
	var rd io.Reader
	if method == "POST" {
		rd = bytes.NewReader([]byte(form))
	}
	req, err := http.NewRequest(method, h.base+path, rd)
	if err != nil {
		return nil, false
	}
	if method == "POST" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return out, true
}

func wireWorld(t *testing.T, sk string) (*stripeWire, *fakeClock, memKV, func(req string) *terminalCharger) {
	t.Helper()
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := &stripeWire{clk: clk, start: clk.t, pis: map[string]*wirePI{}, idem: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	t.Cleanup(srv.Close)
	kv := memKV{}
	mk := func(req string) *terminalCharger {
		return &terminalCharger{api: httpStripe{srv.URL}, clk: clk, kv: kv, sk: sk, currency: "gbp",
			readerID: "tmr_1", requestID: req, amount: 1250, start: clk.t}
	}
	return s, clk, kv, mk
}

func wireOK(t *testing.T, s *stripeWire) {
	t.Helper()
	if len(s.problems) > 0 {
		t.Fatalf("Stripe would reject the plugin's requests: %v", s.problems)
	}
}

func TestWireCreateBodyAndIdempotencyKey(t *testing.T) {
	s, clk, _, mk := wireWorld(t, "sk_live_x")
	s.tapAt = 2 * time.Second
	if out := run(t, mk("att:0:ab"), clk); !out.approved {
		t.Fatalf("want approved, got %+v", out)
	}
	wireOK(t, s)
	c := s.reqs[0]
	if c.method != "POST" || c.path != "/v1/payment_intents" {
		t.Fatalf("first request %s %s", c.method, c.path)
	}
	for k, want := range map[string]string{
		"amount": "1250", "currency": "gbp", "payment_method_types[]": "card_present",
		"capture_method": "automatic", "metadata[ut_request_id]": "att:0:ab",
	} {
		if got := c.form.Get(k); got != want {
			t.Errorf("create %s = %q, want %q", k, got, want)
		}
	}
	if c.idem != "ut-att:0:ab-0" {
		t.Errorf("Idempotency-Key = %q", c.idem)
	}
	pr := s.reqs[1]
	if pr.path != "/v1/terminal/readers/tmr_1/process_payment_intent" || pr.form.Get("payment_intent") != "pi_a" {
		t.Errorf("second request %s form %v", pr.path, pr.form)
	}
}

// The simulated-card helper exists only in test mode: a live key must never
// call it.
func TestWireTestHelperOnlyWithTestKey(t *testing.T) {
	for sk, want := range map[string]int{"sk_test_x": 1, "sk_live_x": 0} {
		s, clk, _, mk := wireWorld(t, sk)
		s.tapAt = 2 * time.Second
		if out := run(t, mk("att:0:ab"), clk); !out.approved {
			t.Fatalf("%s: want approved, got %+v", sk, out)
		}
		if s.helper != want {
			t.Errorf("%s: present_payment_method called %d times, want %d", sk, s.helper, want)
		}
	}
}

// A card declined on the reader is declined at once with Stripe's own
// reason, not after the 18 s wait (#6440), and nothing stays collectable.
func TestWireReaderDeclineIsImmediate(t *testing.T) {
	s, clk, _, mk := wireWorld(t, "sk_live_x")
	s.declineAt = 3 * time.Second
	out := run(t, mk("att:0:ab"), clk)
	wireOK(t, s)
	if out.approved || out.reason != "insufficient_funds" {
		t.Fatalf("got %+v, want declined insufficient_funds", out)
	}
	if took := clk.now().Sub(s.start); took > 8*time.Second {
		t.Fatalf("decline took %s, must not wait out the poll", took)
	}
	if s.pis["pi_a"].status != "canceled" || s.pis["pi_a"].onReader {
		t.Fatalf("PaymentIntent left collectable: %+v", s.pis["pi_a"])
	}
}

// Nobody taps: reader action and PaymentIntent are cancelled, so a late tap
// cannot charge.
func TestWireNoTapCancelsEverything(t *testing.T) {
	s, clk, _, mk := wireWorld(t, "sk_live_x")
	out := run(t, mk("att:0:ab"), clk)
	wireOK(t, s)
	if out.approved || out.reason != "reader_timeout" {
		t.Fatalf("got %+v, want reader_timeout", out)
	}
	sawCancelAction := false
	for _, r := range s.reqs {
		if strings.HasSuffix(r.path, "/cancel_action") {
			sawCancelAction = true
		}
	}
	if !sawCancelAction {
		t.Fatal("the reader prompt was never cancelled")
	}
	if p := s.pis["pi_a"]; p.status != "canceled" || p.onReader {
		t.Fatalf("PaymentIntent %+v still chargeable", p)
	}
}

// Stripe replays a stored error for the same Idempotency-Key for 24 h; the
// plugin must move to a new key on the retry so the sale can start.
func TestWireStoredErrorReplayUsesNewKeyOnRetry(t *testing.T) {
	s, clk, _, mk := wireWorld(t, "sk_live_x")
	s.createError = true
	if out := run(t, mk("att:0:ab"), clk); out.approved || out.reason != "create_failed" {
		t.Fatalf("first: %+v", out)
	}
	s.createError = false
	s.tapAt = 2 * time.Second
	s.start = clk.now()
	c := mk("att:0:ab")
	c.start = clk.now()
	if out := run(t, c, clk); !out.approved {
		t.Fatalf("retry after a stored error: %+v", out)
	}
	var keys []string
	for _, r := range s.reqs {
		if r.path == "/v1/payment_intents" && r.method == "POST" {
			keys = append(keys, r.idem)
		}
	}
	if len(keys) != 2 || keys[0] == keys[1] {
		t.Fatalf("idempotency keys %v: the retry must use a new key", keys)
	}
}

func TestWireRefundBodyAndOutcomes(t *testing.T) {
	s, clk, _, mk := wireWorld(t, "sk_live_x")
	s.tapAt = 2 * time.Second
	c := mk("att:0:ab")
	out := run(t, c, clk)
	if !out.approved {
		t.Fatalf("setup %+v", out)
	}
	id, done, tooLarge, reason := refundOne(c.api, out.authCode, 500, "sale1")
	if !done || tooLarge || reason != "" || id != "re_1" {
		t.Fatalf("refund = %q %v %v %q", id, done, tooLarge, reason)
	}
	if f := s.refunds[0]; f.Get("payment_intent") != out.authCode || f.Get("amount") != "500" {
		t.Fatalf("refund form %v", f)
	}
	// A PaymentIntent that never succeeded cannot be refunded: a decline.
	s.pis["pi_x"] = &wirePI{status: "canceled"}
	if _, done, _, reason := refundOne(c.api, "pi_x", 100, "sale2"); done || reason != "charge_not_refundable" {
		t.Fatalf("refund of an uncharged PI: done=%v reason=%q", done, reason)
	}
}
