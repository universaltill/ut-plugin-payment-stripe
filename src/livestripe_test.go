package main

// Live Stripe test-mode client and its guards (ut-docs#7081). This file is
// UNTAGGED so every `go test` compiles it and its offline unit tests run
// without a network; the scenarios that use it live in
// simreader_live_test.go behind the `stripelive` build tag.
//
// Safety rules (docs/simulated-reader.md):
//   - only an sk_test_ secret key is accepted (rk_test_ is refused too: the
//     flow sends present_payment_method only for sk_test_, see terminal.go);
//   - the client reaches https://api.stripe.com and nothing else;
//   - neither the key nor Stripe's error message is ever printed (the message
//     echoes the key, ut-docs#6570): failures go through errSummary.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode"
)

const (
	liveKeyEnv        = "STRIPE_TEST_TERMINAL_KEY"
	liveStripeVersion = "2025-05-28.basil" // first version with simulated_stripe_s700
	liveBase          = "https://api.stripe.com"
)

var errNotTestKey = errors.New(liveKeyEnv + " must be a Stripe TEST secret key (sk_test_...); refusing")

// checkLiveKey is nil only for an sk_test_ key with something after the
// prefix and no whitespace anywhere.
func checkLiveKey(k string) error {
	const prefix = "sk_test_"
	if !strings.HasPrefix(k, prefix) || len(k) <= len(prefix) {
		return errNotTestKey
	}
	for _, r := range k {
		if unicode.IsSpace(r) {
			return errNotTestKey
		}
	}
	return nil
}

// stripeHostOnly refuses (without forwarding) any request that is not
// https://api.stripe.com.
type stripeHostOnly struct{ next http.RoundTripper }

func (s stripeHostOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL == nil || r.URL.Scheme != "https" || r.URL.Host != "api.stripe.com" {
		return nil, errors.New("live test client: only https://api.stripe.com is allowed")
	}
	return s.next.RoundTrip(r)
}

type liveStripe struct {
	key string
	hc  *http.Client
}

// newLiveStripe returns errNotTestKey before building any client when the
// key is not an sk_test_ key. rt == nil uses http.DefaultTransport.
func newLiveStripe(key string, rt http.RoundTripper) (*liveStripe, error) {
	if err := checkLiveKey(key); err != nil {
		return nil, err
	}
	if rt == nil {
		rt = http.DefaultTransport
	}
	return &liveStripe{key: key, hc: &http.Client{Timeout: 5 * time.Second, Transport: stripeHostOnly{rt}}}, nil
}

// call implements stripeAPI against real Stripe (or a RoundTripper in unit
// tests). A transport error returns (nil, 0, false); the error is never
// printed because it could carry request details.
func (l *liveStripe) call(method, path, form, idemKey string) ([]byte, int, bool) {
	if checkLiveKey(l.key) != nil {
		return nil, 0, false
	}
	target := liveBase + path
	var rd io.Reader
	if method == "POST" {
		rd = bytes.NewReader([]byte(form))
	} else if form != "" {
		target += "?" + form
	}
	req, err := http.NewRequest(method, target, rd)
	if err != nil {
		return nil, 0, false
	}
	req.Header.Set("Authorization", "Bearer "+l.key)
	req.Header.Set("Stripe-Version", liveStripeVersion)
	if method == "POST" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := l.hc.Do(req)
	if err != nil {
		return nil, 0, false
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return out, resp.StatusCode, true
}

// errSummary is the only way test code describes a Stripe failure. It drops
// Message (it echoes the key) and never returns the raw body.
func errSummary(body []byte, status int) string {
	var e struct {
		Error stripeError `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	if status == 0 {
		return "no reply (transport error)"
	}
	return fmt.Sprintf("status=%d code=%s type=%s", status, e.Error.Code, e.Error.Type)
}

// presentOverride decorates a stripeAPI and changes ONLY the test-helper
// present_payment_method call: suppress it (no tap), or send body instead of
// the plugin's empty form (e.g. a decline card number).
type presentOverride struct {
	inner    stripeAPI
	body     string
	suppress bool
	presents int
}

func (p *presentOverride) call(method, path, form, idemKey string) ([]byte, int, bool) {
	if method == "POST" && strings.HasPrefix(path, "/v1/test_helpers/terminal/readers/") && strings.HasSuffix(path, "/present_payment_method") {
		p.presents++
		if p.suppress {
			return []byte(`{}`), 200, true
		}
		return p.inner.call(method, path, p.body, idemKey)
	}
	return p.inner.call(method, path, form, idemKey)
}

// wallClock is the real clock (main.go's realClock is wasip1-only).
type wallClock struct{}

func (wallClock) now() time.Time        { return time.Now() }
func (wallClock) sleep(d time.Duration) { time.Sleep(d) }

// ---- offline unit tests ----

type failRT struct{ used int }

func (f *failRT) RoundTrip(*http.Request) (*http.Response, error) {
	f.used++
	return nil, errors.New("must not be used")
}

type recRT struct{ reqs []*http.Request }

func (r *recRT) RoundTrip(q *http.Request) (*http.Response, error) {
	r.reqs = append(r.reqs, q)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}, Request: q}, nil
}

func TestLiveKeyGuardRefusesNonTestKeys(t *testing.T) {
	for _, k := range []string{"", "sk_live_x", "rk_live_x", "rk_test_x", "pk_test_x", "sk_test_", " sk_test_x", "sk_test_x\n"} {
		rt := &failRT{}
		l, err := newLiveStripe(k, rt)
		if err != errNotTestKey || l != nil {
			t.Errorf("key %q: want errNotTestKey, got %v", k, err)
			continue
		}
		if rt.used != 0 {
			t.Errorf("key %q: transport used", k)
		}
		if len(k) > 8 && strings.Contains(err.Error(), k) {
			t.Errorf("error leaks the key")
		}
	}
	if _, err := newLiveStripe("sk_test_FAKE123", &failRT{}); err != nil {
		t.Errorf("sk_test_ key refused: %v", err)
	}
}

func TestLiveClientHeadersAndHost(t *testing.T) {
	rt := &recRT{}
	l, err := newLiveStripe("sk_test_FAKE123", rt)
	if err != nil {
		t.Fatal(err)
	}
	if _, st, ok := l.call("POST", "/v1/payment_intents", "amount=1000", "idem-1"); !ok || st != 200 {
		t.Fatalf("post: %d %v", st, ok)
	}
	if _, st, ok := l.call("GET", "/v1/terminal/readers", "limit=100", ""); !ok || st != 200 {
		t.Fatalf("get: %d %v", st, ok)
	}
	if len(rt.reqs) != 2 {
		t.Fatalf("want 2 requests, got %d", len(rt.reqs))
	}
	for _, r := range rt.reqs {
		if r.URL.Scheme != "https" || r.URL.Host != "api.stripe.com" {
			t.Errorf("target %s://%s", r.URL.Scheme, r.URL.Host)
		}
		if r.Header.Get("Authorization") != "Bearer sk_test_FAKE123" {
			t.Errorf("authorization header wrong")
		}
		if r.Header.Get("Stripe-Version") != "2025-05-28.basil" {
			t.Errorf("stripe-version %q", r.Header.Get("Stripe-Version"))
		}
	}
	post, get := rt.reqs[0], rt.reqs[1]
	if post.Header.Get("Idempotency-Key") != "idem-1" {
		t.Errorf("idempotency key not passed")
	}
	if post.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		t.Errorf("post content type %q", post.Header.Get("Content-Type"))
	}
	if b, _ := io.ReadAll(post.Body); string(b) != "amount=1000" {
		t.Errorf("post body %q", b)
	}
	if get.URL.RawQuery != "limit=100" {
		t.Errorf("get query %q", get.URL.RawQuery)
	}
	if get.Header.Get("Content-Type") != "" {
		t.Errorf("GET must not send a form content type")
	}
}

func TestLiveClientRefusesOtherHosts(t *testing.T) {
	rt := &recRT{}
	g := stripeHostOnly{rt}
	for _, u := range []string{
		"http://api.stripe.com/x",
		"https://evil.example/x",
		"https://api.stripe.com.evil.example/x",
		"https://evil.example/https://api.stripe.com/x",
		"https://evilstripe.com/x",
		"https://xapi.stripe.com/x",
		"https://stripe.com/x",
		"https://api.stripe.com:8443/x",
		"https://api.stripe.com@evil.example/x",
	} {
		req, _ := http.NewRequest("GET", u, nil)
		if _, err := g.RoundTrip(req); err == nil {
			t.Errorf("%s: want error", u)
		}
	}
	if len(rt.reqs) != 0 {
		t.Errorf("a refused request was forwarded")
	}
}

// The guard must be wired into the client: a path that would redirect the
// host ("https://api.stripe.com" + "@evil.example/x") reaches nothing.
func TestLiveStripeForwardsNothingWhenPathRedirectsHost(t *testing.T) {
	rt := &recRT{}
	l, err := newLiveStripe("sk_test_FAKE123", rt)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"GET", "POST"} {
		if _, st, ok := l.call(m, "@evil.example/x", "", ""); ok || st != 0 {
			t.Errorf("%s: want refusal, got status=%d ok=%v", m, st, ok)
		}
	}
	if len(rt.reqs) != 0 {
		t.Errorf("a host-redirecting path was forwarded to %s", rt.reqs[0].URL.Host)
	}
}

func TestErrSummaryDropsMessage(t *testing.T) {
	body := []byte(`{"error":{"code":"x","type":"invalid_request_error","message":"Invalid API Key provided: sk_test_****abcd"}}`)
	got := errSummary(body, 401)
	if got != "status=401 code=x type=invalid_request_error" {
		t.Errorf("got %q", got)
	}
	if strings.Contains(got, "sk_") {
		t.Errorf("summary leaks key text")
	}
	if s := errSummary([]byte("<html>sk_test_zzz</html>"), 502); strings.Contains(s, "sk_") {
		t.Errorf("raw body leaked: %q", s)
	}
}

type callRec struct{ method, path, form, idem string }
type recAPI struct{ calls []callRec }

func (r *recAPI) call(method, path, form, idem string) ([]byte, int, bool) {
	r.calls = append(r.calls, callRec{method, path, form, idem})
	return []byte(`{"ok":1}`), 200, true
}

func TestPresentOverrideTouchesOnlyPresent(t *testing.T) {
	const present = "/v1/test_helpers/terminal/readers/tmr_1/present_payment_method"
	in := &recAPI{}
	sup := &presentOverride{inner: in, suppress: true}
	if b, st, ok := sup.call("POST", present, "", ""); string(b) != "{}" || st != 200 || !ok {
		t.Errorf("suppressed reply %q %d %v", b, st, ok)
	}
	if len(in.calls) != 0 || sup.presents != 1 {
		t.Errorf("suppress forwarded=%d presents=%d", len(in.calls), sup.presents)
	}

	in = &recAPI{}
	rw := &presentOverride{inner: in, body: "type=card_present&card_present[number]=4000000000000002"}
	rw.call("POST", present, "", "k")
	if len(in.calls) != 1 || in.calls[0].form != rw.body || in.calls[0].idem != "k" || rw.presents != 1 {
		t.Errorf("rewrite: %+v presents=%d", in.calls, rw.presents)
	}

	in = &recAPI{}
	pt := &presentOverride{inner: in, body: "x=1", suppress: true}
	others := []callRec{
		{"POST", "/v1/payment_intents", "amount=1000", "i1"},
		{"GET", "/v1/terminal/readers/tmr_1", "", ""},
		{"POST", "/v1/terminal/readers/tmr_1/cancel_action", "", ""},
		{"GET", present, "", ""}, // a GET is not the present call
	}
	for _, c := range others {
		pt.call(c.method, c.path, c.form, c.idem)
	}
	if len(in.calls) != len(others) || pt.presents != 0 {
		t.Fatalf("forwarded %d of %d, presents=%d", len(in.calls), len(others), pt.presents)
	}
	for i, c := range others {
		if in.calls[i] != c {
			t.Errorf("call %d changed: %+v", i, in.calls[i])
		}
	}
}
