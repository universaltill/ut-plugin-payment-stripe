package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ut-docs#6566: the till may name the reader per call (payload reader_id).

func TestChooseReaderPayloadWinsOverSetting(t *testing.T) {
	id, reason := chooseReader("tmr_payload", "tmr_setting")
	if id != "tmr_payload" || reason != "" {
		t.Fatalf("got %q %q", id, reason)
	}
}

func TestChooseReaderFallsBackToSettingWhenMissing(t *testing.T) {
	for _, p := range []string{"", "   "} {
		id, reason := chooseReader(p, "tmr_setting")
		if id != "tmr_setting" || reason != "" {
			t.Fatalf("payload %q: got %q %q", p, id, reason)
		}
	}
	if id, reason := chooseReader("", ""); id != "" || reason != "" {
		t.Fatalf("no id anywhere must stay the online/demo flow: %q %q", id, reason)
	}
}

func TestChooseReaderRejectsMalformedNoFallback(t *testing.T) {
	bad := []string{
		"tmr_1/../x", "../", "tmr_", "tmr_" + strings.Repeat("a", 65), "rdr_ABC",
		"tmr_a b", "tmr_a?x=1", "tmr_a\n", "tmr_é", "TMR_abc", "tmr_a/b", "tmr_a%2fb",
	}
	for _, p := range bad {
		id, reason := chooseReader(p, "tmr_setting")
		if id != "" || reason != "invalid_reader" {
			t.Errorf("payload %q: got %q %q, want decline invalid_reader and no fallback", p, id, reason)
		}
	}
	if _, reason := chooseReader("tmr_"+strings.Repeat("a", 64), ""); reason != "" {
		t.Errorf("64 chars must be accepted, got %q", reason)
	}
}

func TestEventPayloadCarriesReaderID(t *testing.T) {
	ev := parseEvent([]byte(`{"id":"e1","type":"payment.stripe.authorize","payload":{"amount":1250,"currency":"GBP","reader_id":"tmr_P","reader_kind":"cloud"}}`))
	if ev.Payload.ReaderID != "tmr_P" || ev.Payload.Amount != 1250 {
		t.Fatalf("%+v", ev.Payload)
	}
}

// The payload reader reaches the provider URL path, not the setting's.
func TestWirePayloadReaderUsedInURL(t *testing.T) {
	s, clk, _, mk := wireWorld(t, "sk_live_x")
	s.tapAt = 2 * time.Second
	c := mk("att:0:ab")
	id, reason := chooseReader("tmr_PAYLOAD9", "tmr_1")
	if reason != "" {
		t.Fatal(reason)
	}
	c.readerID = id
	if out := run(t, c, clk); !out.approved {
		t.Fatalf("got %+v", out)
	}
	found := false
	for _, r := range s.reqs {
		if strings.Contains(r.path, "tmr_1") {
			t.Errorf("setting reader used: %s", r.path)
		}
		if strings.HasPrefix(r.path, "/v1/terminal/readers/tmr_PAYLOAD9/") {
			found = true
		}
	}
	if !found {
		t.Fatalf("payload reader never called: %+v", s.reqs)
	}
}

// Refund stays by transaction_ref: a reader_id on a leg is accepted and never
// blocks, even a malformed one.
func TestRefundIgnoresReaderIDOnLegs(t *testing.T) {
	raw := `{"id":"evt-9","type":"payment.stripe.refund","payload":{"amount":500,"original_payments":[{"transaction_ref":"pi_sale","amount":1250,"reader_id":"../evil"}],"reader_id":"tmr_gone"}}`
	ev := parseEvent([]byte(raw))
	if ev.Payload.OriginalPayments[0].ReaderID != "../evil" {
		t.Fatalf("leg reader not parsed: %+v", ev.Payload.OriginalPayments)
	}
	api := newLedger(map[string]int64{"pi_sale": 1250})
	if _, reason := refundSale(api, memKV{}, noLinks(), ev.Payload); reason != "" {
		t.Fatalf("refund declined %q", reason)
	}
	if api.refunded["pi_sale"] != 500 || api.idemKeys[0] != "ut-refund-evt-9-pi_sale" {
		t.Fatalf("%v %q", api.refunded, api.idemKeys)
	}
}

// ---- .readers state hook ----

type readerFake struct {
	calls []string
	reply map[string]struct {
		status int
		body   string
		ok     bool
	}
}

func (f *readerFake) call(method, path, form, idem string) ([]byte, int, bool) {
	f.calls = append(f.calls, method+" "+path)
	id := strings.TrimPrefix(path, "/v1/terminal/readers/")
	r, found := f.reply[id]
	if !found {
		return []byte(`{"error":{"code":"resource_missing"}}`), 404, true
	}
	return []byte(r.body), r.status, r.ok
}

func rep(status int, body string) struct {
	status int
	body   string
	ok     bool
} {
	return struct {
		status int
		body   string
		ok     bool
	}{status, body, true}
}

func TestReaderStatesMapping(t *testing.T) {
	f := &readerFake{reply: map[string]struct {
		status int
		body   string
		ok     bool
	}{
		"tmr_ready":   rep(200, `{"id":"tmr_ready","status":"online","action":null}`),
		"tmr_noact":   rep(200, `{"id":"tmr_noact","status":"online"}`),
		"tmr_busy":    rep(200, `{"id":"tmr_busy","status":"online","action":{"status":"in_progress"}}`),
		"tmr_off":     rep(200, `{"id":"tmr_off","status":"offline","action":null}`),
		"tmr_http500": rep(500, `{"error":{"message":"sk_live_secret"}}`),
		"tmr_weird":   rep(200, `{"status":"sleeping"}`),
		"tmr_json":    rep(200, `not json`),
		"tmr_net":     {0, "", false},
		"tmr_401":     rep(401, invalidKeyBody),
	}}
	refs := []string{"tmr_ready", "tmr_noact", "tmr_busy", "tmr_off", "tmr_gone", "tmr_http500", "tmr_weird", "tmr_json", "tmr_net", "tmr_401"}
	got := readerStates(f, refs)
	want := map[string][2]string{
		"tmr_ready": {"ready", ""}, "tmr_noact": {"ready", ""}, "tmr_busy": {"busy", ""},
		"tmr_off": {"offline", ""}, "tmr_gone": {"offline", "reader_unreachable"},
		"tmr_http500": {"unknown", ""}, "tmr_weird": {"unknown", ""}, "tmr_json": {"unknown", ""},
		"tmr_net": {"unknown", ""}, "tmr_401": {"unknown", "invalid_secret_key"},
	}
	if len(got) != len(refs) {
		t.Fatalf("got %d rows", len(got))
	}
	for i, r := range got {
		if r.ReaderID != refs[i] {
			t.Errorf("order: %q at %d", r.ReaderID, i)
		}
		w := want[r.ReaderID]
		if r.State != w[0] || r.Reason != w[1] {
			t.Errorf("%s: %s/%s, want %s/%s", r.ReaderID, r.State, r.Reason, w[0], w[1])
		}
	}
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "GET /v1/terminal/readers/") {
			t.Errorf("state check must be read-only GET: %q", c)
		}
	}
}

func TestReaderStatesTruncatesAndNeverCallsMalformed(t *testing.T) {
	f := &readerFake{}
	var refs []string
	for i := 0; i < 20; i++ {
		refs = append(refs, "tmr_r"+string(rune('a'+i)))
	}
	refs[0] = "tmr_bad/../x"
	got := readerStates(f, refs)
	if len(got) != maxStateReaders {
		t.Fatalf("got %d rows, want cap %d", len(got), maxStateReaders)
	}
	if got[0].State != "unknown" || got[0].Reason != "invalid_reader" {
		t.Fatalf("malformed: %+v", got[0])
	}
	if len(f.calls) != maxStateReaders-1 {
		t.Fatalf("%d calls", len(f.calls))
	}
	for _, c := range f.calls {
		if strings.Contains(c, "..") {
			t.Fatalf("malformed ref reached a URL: %q", c)
		}
	}
}

func TestReadersAnswerJSONAndRequestParse(t *testing.T) {
	ev := parseEvent([]byte(`{"id":"e","type":"payment.stripe.readers","payload":{"op":"state","plugin_id":"p","readers":["tmr_a","tmr_b"]}}`))
	if ev.Payload.Op != "state" || len(ev.Payload.Readers) != 2 {
		t.Fatalf("%+v", ev.Payload)
	}
	f := &readerFake{reply: map[string]struct {
		status int
		body   string
		ok     bool
	}{"tmr_a": rep(200, `{"status":"online","action":null}`)}}
	b := readersAnswer(f, ev.Payload)
	var out struct {
		Readers []map[string]string `json:"readers"`
	}
	if err := json.Unmarshal(b, &out); err != nil || len(out.Readers) != 2 {
		t.Fatalf("%s %v", b, err)
	}
	if out.Readers[0]["reader_id"] != "tmr_a" || out.Readers[0]["state"] != "ready" {
		t.Fatalf("%s", b)
	}
	if _, has := out.Readers[0]["reason"]; has {
		t.Fatalf("empty reason must be omitted: %s", b)
	}
}

func TestReadersAnswerOtherOpIsEmpty(t *testing.T) {
	b := readersAnswer(&readerFake{}, eventPayload{Op: "write", Readers: []string{"tmr_a"}})
	if string(b) != `{"readers":[]}` {
		t.Fatalf("%s", b)
	}
}

func TestReaderStateLogsMaskedOnly(t *testing.T) {
	logs := captureLogs(t)
	f := &readerFake{reply: map[string]struct {
		status int
		body   string
		ok     bool
	}{"tmr_SECRETREADER123": rep(401, invalidKeyBody)}}
	readerStates(f, []string{"tmr_SECRETREADER123"})
	for _, l := range *logs {
		if strings.Contains(l, "SECRETREADER") || strings.Contains(l, "sk_test") {
			t.Fatalf("log leaks: %q", l)
		}
	}
}
