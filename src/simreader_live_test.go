//go:build stripelive

package main

// Live simulated S700 run against Stripe TEST mode (ut-docs#7081). Opt-in:
// build tag `stripelive` AND env STRIPE_TEST_TERMINAL_KEY (an sk_test_ key);
// without the key every test skips green. Drives the plugin's real
// terminalCharger / refundSale / refundLegs / readerStates code over real
// HTTP with a real clock. Failure text goes through errSummary only: never
// the key, never Stripe's message, never a transport error.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func liveAPI(t *testing.T) (*liveStripe, string) {
	t.Helper()
	key := os.Getenv(liveKeyEnv)
	if key == "" {
		t.Skip("skipped: no " + liveKeyEnv)
	}
	api, err := newLiveStripe(key, nil)
	if err != nil {
		t.Fatal(err) // errNotTestKey carries no key text
	}
	return api, key
}

// captureLiveLogs collects the plugin's log lines and fails if one has the key.
func captureLiveLogs(t *testing.T, key string) *[]string {
	t.Helper()
	var logs []string
	logHook = func(m string) { logs = append(logs, m) }
	t.Cleanup(func() {
		logHook = nil
		for _, l := range logs {
			if strings.Contains(l, key) {
				t.Errorf("a plugin log line contains the key")
				return
			}
		}
	})
	return &logs
}

// getJSON GETs path into v; on failure it fails with errSummary text only.
func getJSON(t *testing.T, api *liveStripe, path, form string, v any) {
	t.Helper()
	body, st, ok := api.call("GET", path, form, "")
	if !ok || st != 200 {
		t.Fatalf("GET %s: %s", path, errSummary(body, st))
	}
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("GET %s: unreadable reply", path)
	}
}

func accountInfo(t *testing.T, api *liveStripe) (currency, country string) {
	t.Helper()
	var a struct {
		DefaultCurrency string `json:"default_currency"`
		Country         string `json:"country"`
	}
	getJSON(t, api, "/v1/account", "", &a)
	return a.DefaultCurrency, a.Country
}

func accountCurrency(t *testing.T, api *liveStripe) string {
	t.Helper()
	cur, _ := accountInfo(t, api)
	if cur == "" {
		t.Fatal("account has no default_currency")
	}
	return cur
}

var locationAddresses = map[string]map[string]string{
	"US": {"line1": "1 Test Street", "city": "San Francisco", "state": "CA", "postal_code": "94103"},
	"GB": {"line1": "1 Test Street", "city": "London", "postal_code": "EC1A 1BB"},
	"IE": {"line1": "1 Test Street", "city": "Dublin", "postal_code": "D01 F5P2"},
	"DE": {"line1": "Teststrasse 1", "city": "Berlin", "postal_code": "10115"},
	"FR": {"line1": "1 rue de Test", "city": "Paris", "postal_code": "75001"},
	"NL": {"line1": "Teststraat 1", "city": "Amsterdam", "postal_code": "1011 AA"},
	"CA": {"line1": "1 Test Street", "city": "Toronto", "state": "ON", "postal_code": "M5V 2T6"},
	"AU": {"line1": "1 Test Street", "city": "Sydney", "state": "NSW", "postal_code": "2000"},
}

// liveLocation: env override, else reuse a location tagged metadata ut_ci,
// else create one. Locations are kept for reuse, never deleted.
func liveLocation(t *testing.T, api *liveStripe) string {
	t.Helper()
	if env := os.Getenv("STRIPE_TEST_TERMINAL_LOCATION"); env != "" {
		if !strings.HasPrefix(env, "tml_") {
			t.Fatal("STRIPE_TEST_TERMINAL_LOCATION must start with tml_")
		}
		return env
	}
	var list struct {
		Data []struct {
			ID       string            `json:"id"`
			Metadata map[string]string `json:"metadata"`
		} `json:"data"`
	}
	getJSON(t, api, "/v1/terminal/locations", "limit=100", &list)
	for _, l := range list.Data {
		if l.Metadata["ut_ci"] == "simreader" {
			return l.ID
		}
	}
	_, country := accountInfo(t, api)
	addr, known := locationAddresses[country]
	if !known {
		t.Fatal("no built-in test address for this account country: set STRIPE_TEST_TERMINAL_LOCATION")
	}
	v := url.Values{}
	v.Set("display_name", "test-7081-simulated-reader")
	v.Set("metadata[ut_ci]", "simreader")
	v.Set("address[country]", country)
	for k, val := range addr {
		v.Set("address["+k+"]", val)
	}
	form := v.Encode()
	body, st, ok := api.call("POST", "/v1/terminal/locations", form, "")
	var created struct {
		ID string `json:"id"`
	}
	if !ok || st != 200 || json.Unmarshal(body, &created) != nil || !strings.HasPrefix(created.ID, "tml_") {
		t.Fatalf("create location: %s", errSummary(body, st))
	}
	return created.ID
}

// sweepStaleReaders deletes leftover ut_ci readers (a run killed by a
// timeout skips t.Cleanup). The workflow concurrency group keeps this safe.
func sweepStaleReaders(t *testing.T, api *liveStripe, loc string) {
	t.Helper()
	var list struct {
		Data []struct {
			ID       string            `json:"id"`
			Metadata map[string]string `json:"metadata"`
		} `json:"data"`
	}
	getJSON(t, api, "/v1/terminal/readers", "limit=100&location="+url.QueryEscape(loc), &list)
	for _, r := range list.Data {
		if r.Metadata["ut_ci"] == "simreader" && validReaderID(r.ID) {
			deleteReader(t, api, r.ID)
		}
	}
}

// deleteReader is best-effort cleanup: a failed delete is logged with
// errSummary only and never fails the test.
func deleteReader(t *testing.T, api *liveStripe, id string) {
	t.Helper()
	body, st, ok := api.call("DELETE", "/v1/terminal/readers/"+id, "", "")
	if !ok {
		t.Logf("cleanup: delete reader %s: no reply", id)
		return
	}
	if st < 200 || st > 299 {
		t.Logf("cleanup: delete reader %s: %s", id, errSummary(body, st))
	}
}

// registerSimS700 registers a simulated S700 and queues its deletion at once,
// so a later t.Fatal still cleans up.
func registerSimS700(t *testing.T, api *liveStripe, loc, runID string) string {
	t.Helper()
	form := "registration_code=simulated-s700&location=" + url.QueryEscape(loc) +
		"&label=" + url.QueryEscape("test-7081-simreader-"+runID) + "&metadata[ut_ci]=simreader"
	body, st, ok := api.call("POST", "/v1/terminal/readers", form, "")
	var rd struct {
		ID         string `json:"id"`
		DeviceType string `json:"device_type"`
	}
	if !ok || st != 200 || json.Unmarshal(body, &rd) != nil || !validReaderID(rd.ID) {
		t.Fatalf("register simulated reader: %s", errSummary(body, st))
	}
	t.Cleanup(func() { deleteReader(t, api, rd.ID) })
	if rd.DeviceType != "simulated_stripe_s700" {
		t.Fatalf("API version/account does not offer simulated_stripe_s700 (device_type=%q)", rd.DeviceType)
	}
	return rd.ID
}

func liveCharger(api stripeAPI, kv kvStore, key, cur, reader, req string, amount int64) *terminalCharger {
	return &terminalCharger{api, wallClock{}, kv, key, cur, reader, req, amount, time.Now()}
}

func piStatus(t *testing.T, api *liveStripe, pi string) string {
	t.Helper()
	var p struct {
		Status string `json:"status"`
	}
	getJSON(t, api, "/v1/payment_intents/"+pi, "", &p)
	return p.Status
}

func TestSimulatedS700(t *testing.T) {
	api, key := liveAPI(t)
	captureLiveLogs(t, key)
	runID := fmt.Sprintf("ci%d", time.Now().UnixNano())
	cur := accountCurrency(t, api)
	loc := liveLocation(t, api)
	sweepStaleReaders(t, api, loc)
	reader := registerSimS700(t, api, loc, runID)
	kv := memKV{}

	readyCheck := func(t *testing.T) {
		t.Helper()
		var got []readerState
		for i := 0; i < 5; i++ {
			got = readerStates(api, []string{reader})
			if len(got) == 1 && got[0].State == "ready" {
				return
			}
			time.Sleep(time.Second)
		}
		t.Fatalf("reader state %+v, want ready", got)
	}

	t.Run("reader_state_ready", func(t *testing.T) {
		readyCheck(t)
		if id, reason := chooseReader(reader, ""); id != reader || reason != "" {
			t.Errorf("chooseReader = %q %q", id, reason)
		}
	})

	var saleTxn string
	t.Run("sale_approved", func(t *testing.T) {
		po := &presentOverride{inner: api}
		out := liveCharger(po, kv, key, cur, reader, runID+"-ok", 1000).run()
		if !out.approved || !strings.HasPrefix(out.authCode, "pi_") {
			t.Fatalf("not approved: reason=%q", out.reason)
		}
		saleTxn = out.authCode
		if po.presents != 1 {
			t.Errorf("presents=%d", po.presents)
		}
		if st := piStatus(t, api, saleTxn); st != "succeeded" {
			t.Errorf("PaymentIntent status %q", st)
		}
		// Probe, non-fatal: does a finished action keep the reader busy?
		t.Logf("reader state right after the approved sale: %+v", readerStates(api, []string{reader}))
	})

	// Only the refund subtests depend on the sale; decline, no-tap and the
	// final ready check run either way.
	needSale := func(t *testing.T) {
		t.Helper()
		if saleTxn == "" {
			t.Skip("no approved sale to refund")
		}
	}
	refund := func(amount int64, evt string, ra *int64) (string, string) {
		return refundSale(api, memKV{}, linkStore{blobs: newFakeBlobs()}, eventPayload{
			Amount: amount, EventID: evt,
			OriginalPayments: []origPayment{{TransactionRef: saleTxn, Amount: 1000, RefundAmount: ra}},
		})
	}

	t.Run("refund_partial_legacy", func(t *testing.T) {
		needSale(t)
		id, decl := refund(400, runID+"-rf1", nil)
		if !strings.HasPrefix(id, "re_") || decl != "" {
			t.Fatalf("refund id=%q decline=%q", id, decl)
		}
		if again, _ := refund(400, runID+"-rf1", nil); again != id {
			t.Errorf("replay gave a different refund")
		}
	})

	t.Run("refund_per_leg_rest", func(t *testing.T) {
		needSale(t)
		ra := int64(600)
		id, decl := refund(600, runID+"-rf2", &ra)
		if !strings.HasPrefix(id, "re_") || decl != "" {
			t.Fatalf("refund id=%q decline=%q", id, decl)
		}
	})

	t.Run("refund_over_full_declines", func(t *testing.T) {
		needSale(t)
		id, decl := refund(100, runID+"-rf3", nil)
		if id != "" || (decl != "charge_already_refunded" && decl != "amount_too_large") {
			t.Errorf("refund id=%q decline=%q", id, decl)
		}
		t.Logf("over-refund decline code: %s", decl)
	})

	t.Run("card_declined_early", func(t *testing.T) {
		po := &presentOverride{inner: api, body: "type=card_present&card_present[number]=4000000000000002"}
		req := runID + "-dc"
		began := time.Now()
		out := liveCharger(po, kv, key, cur, reader, req, 1100).run()
		took := time.Since(began)
		if out.approved {
			t.Fatal("a declined card was approved")
		}
		switch out.reason {
		case "generic_decline", "card_declined":
		default:
			t.Fatalf("not a card decline: reason=%q, want generic_decline or card_declined", out.reason)
		}
		t.Logf("decline reason: %s (after %s)", out.reason, took.Round(time.Second))
		if took >= pollBudget {
			t.Errorf("decline took %s, want under %s", took, pollBudget)
		}
		if e, ok := findAttempt(kv, req); !ok || e.Ref == "" {
			t.Error("no attempt recorded")
		} else if st := piStatus(t, api, e.Ref); st != "canceled" {
			t.Errorf("PaymentIntent status %q, want canceled", st)
		}
	})

	t.Run("no_tap_times_out", func(t *testing.T) {
		po := &presentOverride{inner: api, suppress: true}
		req := runID + "-to"
		began := time.Now()
		out := liveCharger(po, kv, key, cur, reader, req, 1200).run()
		took := time.Since(began)
		if out.approved || out.reason != "reader_timeout" {
			t.Fatalf("approved=%v reason=%q, want reader_timeout", out.approved, out.reason)
		}
		if po.presents != 1 {
			t.Errorf("presents=%d", po.presents)
		}
		if took < pollBudget || took > hardDeadline+10*time.Second {
			t.Errorf("timeout after %s, want between %s and %s", took, pollBudget, hardDeadline+10*time.Second)
		}
		if e, ok := findAttempt(kv, req); !ok || e.Ref == "" {
			t.Error("no attempt recorded")
		} else if st := piStatus(t, api, e.Ref); st != "canceled" {
			t.Errorf("PaymentIntent status %q, want canceled", st)
		}
	})

	t.Run("reader_state_ready_after", readyCheck)
}

func TestSimulatedBadKeyIsInvalidKey(t *testing.T) {
	liveAPI(t) // skips the same way when there is no key
	bad, err := newLiveStripe("sk_test_"+strings.Repeat("0", 24), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rs := oneReaderState(bad, "tmr_ABC123"); rs.Reason != declineInvalidKey {
		t.Errorf("reason %q, want %s", rs.Reason, declineInvalidKey)
	}
}
