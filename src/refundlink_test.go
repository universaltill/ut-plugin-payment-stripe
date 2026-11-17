package main

// ut-docs#6438: refunds after the host's 1,024-key plugin-storage cap. The
// fakes enforce the REAL host rules (universal-till internal/data/
// plugin_repo.go StorageSet; internal/plugins/wasm_blob.go).

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	hostMaxKeys  = 1024
	hostMaxKey   = 128
	hostMaxValue = 64 << 10
)

// capKV: 1,024 keys, key 1..128 B, value <= 64 KiB; an EXISTING key can be
// overwritten at the cap, only a NEW key fails (-4).
type capKV struct {
	m       map[string]string
	failAll bool
	puts    int
}

func newCapKV() *capKV { return &capKV{m: map[string]string{}} }

func (c *capKV) get(k string) string { return c.m[k] }
func (c *capKV) put(k string, v []byte) error {
	if c.failAll {
		return fmt.Errorf("storage_set: -3")
	}
	if len(k) < 1 || len(k) > hostMaxKey || len(v) > hostMaxValue {
		return fmt.Errorf("storage_set: -4")
	}
	if _, exists := c.m[k]; !exists && len(c.m) >= hostMaxKeys {
		return fmt.Errorf("storage_set: -4 (key cap)")
	}
	c.puts++
	c.m[k] = string(v)
	return nil
}

var blobNameRe = regexp.MustCompile(`^[a-z0-9._-]{1,128}$`)

type fakeBlobs struct {
	m       map[string][]byte
	quota   int
	deny    bool
	writes  int
	maxSize int
}

func newFakeBlobs() *fakeBlobs { return &fakeBlobs{m: map[string][]byte{}, quota: 50 << 20} }

func (f *fakeBlobs) get(name string) ([]byte, bool, error) {
	if f.deny {
		return nil, false, fmt.Errorf("blob: -2")
	}
	if !blobNameRe.MatchString(name) {
		return nil, false, fmt.Errorf("blob: -4")
	}
	b, ok := f.m[name]
	return append([]byte(nil), b...), ok, nil
}

func (f *fakeBlobs) put(name string, b []byte) error {
	if f.deny {
		return fmt.Errorf("blob: -2")
	}
	if !blobNameRe.MatchString(name) {
		return fmt.Errorf("blob: -4")
	}
	total := len(b)
	for n, v := range f.m {
		if n != name {
			total += len(v)
		}
	}
	if total > f.quota {
		return fmt.Errorf("blob: -5")
	}
	f.writes++
	if len(b) > f.maxSize {
		f.maxSize = len(b)
	}
	f.m[name] = append([]byte(nil), b...)
	return nil
}

// refundAPI records Stripe refund calls. refs maps a PaymentIntent id to its
// scripted answer (default: succeeded).
type refundAPI struct {
	calls   []string
	idems   []string
	answers map[string]string
}

func (r *refundAPI) call(method, path, form, idemKey string) ([]byte, bool) {
	r.calls = append(r.calls, form)
	r.idems = append(r.idems, idemKey)
	pi := strings.SplitN(strings.TrimPrefix(form, "payment_intent="), "&", 2)[0]
	if a, ok := r.answers[pi]; ok {
		return []byte(a), true
	}
	return []byte(`{"id":"re_` + pi + `","status":"succeeded"}`), true
}

func (r *refundAPI) refundedPIs() []string {
	var out []string
	for _, f := range r.calls {
		out = append(out, strings.SplitN(strings.TrimPrefix(f, "payment_intent="), "&", 2)[0])
	}
	return out
}

type world struct {
	raw   *capKV
	blobs *fakeBlobs
	kv    kvStore
	links linkStore
	now   time.Time
	logs  []string
}

func newWorld(t *testing.T) *world {
	w := &world{raw: newCapKV(), blobs: newFakeBlobs(), now: time.Unix(1_800_000_000, 0)}
	w.kv = overflowKV{raw: w.raw, blobs: w.blobs}
	w.links = linkStore{blobs: w.blobs}
	logHook = func(m string) { w.logs = append(w.logs, m) }
	t.Cleanup(func() { logHook = nil })
	return w
}

func saleID(n int) string { return fmt.Sprintf("%08d-0000-4000-8000-%012d", n, n) }

// authorize records an approval the way main.go's approve() does.
func (w *world) authorize(id, ref string, amount, tip int64) {
	saveAuthorizeResult(w.kv, map[string]any{"outcome": "approved", "auth_code": ref},
		&recentEntry{ID: id, Ref: ref, Amount: amount, Tip: tip, At: w.now.Unix()})
}

func (w *world) settle(p eventPayload) { settleSale(w.kv, w.links, p, w.now) }

func (w *world) refund(api *refundAPI, p eventPayload) (string, string) {
	return refundSale(api, w.kv, w.links, p)
}

// sellMany does authorize -> settle (current host: no host fields) n times.
func (w *world) sellMany(n int) {
	for i := 1; i <= n; i++ {
		w.now = w.now.Add(time.Second)
		w.authorize(fmt.Sprintf("auth-%d", i), fmt.Sprintf("pi_%d", i), 1000+int64(i%50), 0)
		w.settle(eventPayload{SaleID: saleID(i), Amount: 1000 + int64(i%50)})
	}
}

func TestP1_1100SalesRefundFirstAndLast(t *testing.T) {
	w := newWorld(t)
	w.sellMany(1100)
	for _, n := range []int{1, 1100, 550} {
		api := &refundAPI{}
		id, dec := w.refund(api, eventPayload{OriginalSaleID: saleID(n), Amount: 1000 + int64(n%50)})
		if dec != "" || id != fmt.Sprintf("re_pi_%d", n) {
			t.Fatalf("sale %d: refund id=%q decline=%q", n, id, dec)
		}
		if got := api.refundedPIs(); len(got) != 1 || got[0] != fmt.Sprintf("pi_%d", n) {
			t.Fatalf("sale %d refunded %v, want only pi_%d", n, got, n)
		}
	}
	for k := range w.raw.m {
		if strings.HasPrefix(k, "sale_pi:") {
			t.Fatalf("per-sale storage key written: %s", k)
		}
	}
}

func TestP2_HostPathWritesNothing(t *testing.T) {
	w := newWorld(t)
	for i := 1; i <= 1100; i++ {
		w.now = w.now.Add(time.Second)
		w.authorize(fmt.Sprintf("auth-%d", i), fmt.Sprintf("pi_%d", i), 1000, 0)
	}
	keys, putsBefore, writesBefore := len(w.raw.m), w.raw.puts, w.blobs.writes
	for i := 1; i <= 1100; i++ {
		w.settle(eventPayload{SaleID: saleID(i), Amount: 1000, TransactionRef: fmt.Sprintf("pi_%d", i), AuthorizeRequestID: fmt.Sprintf("auth-%d", i)})
	}
	if len(w.raw.m) != keys || w.raw.puts != putsBefore || w.blobs.writes != writesBefore {
		t.Fatalf("settle with transaction_ref wrote: keys %d->%d puts %d->%d blobs %d->%d", keys, len(w.raw.m), putsBefore, w.raw.puts, writesBefore, w.blobs.writes)
	}
	for _, n := range []int{1, 1100} {
		api := &refundAPI{}
		ref := fmt.Sprintf("pi_%d", n)
		id, dec := w.refund(api, eventPayload{OriginalSaleID: saleID(n), Amount: 1000,
			OriginalPayments: []origPayment{{TransactionRef: ref, Amount: 1000}}})
		if dec != "" || id != "re_"+ref || len(api.calls) != 1 {
			t.Fatalf("host path refund %d: id=%q dec=%q calls=%v", n, id, dec, api.calls)
		}
	}
}

func TestP3_10000SalesStayBounded(t *testing.T) {
	w := newWorld(t)
	w.sellMany(10000)
	if len(w.raw.m) > 2 {
		t.Fatalf("storage keys = %d, want <= 2 (last_txn, attempts)", len(w.raw.m))
	}
	buckets := 0
	for name, b := range w.blobs.m {
		if !strings.HasPrefix(name, "rl-") {
			t.Fatalf("unexpected blob %s", name)
		}
		buckets++
		if len(b) > linkBucketMaxBytes {
			t.Fatalf("bucket %s is %d B > %d", name, len(b), linkBucketMaxBytes)
		}
	}
	if buckets > linkBuckets {
		t.Fatalf("%d buckets > %d", buckets, linkBuckets)
	}
	for n := 9001; n <= 10000; n += 37 {
		api := &refundAPI{}
		if _, dec := w.refund(api, eventPayload{OriginalSaleID: saleID(n), Amount: 1000 + int64(n%50)}); dec != "" {
			t.Fatalf("recent sale %d declined: %s", n, dec)
		}
	}
}

func TestBucketEvictsOldestWhenFull(t *testing.T) {
	blobs := newFakeBlobs()
	ls := linkStore{blobs: blobs}
	at := time.Unix(1_800_000_000, 0)
	// Force everything into one bucket by finding ids that hash to rl-00.
	var ids []string
	for i := 0; len(ids) < 700; i++ {
		id := fmt.Sprintf("%08d-0000-4000-8000-%012d", i, i)
		if bucketName(id) == "rl-00" {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		if err := ls.add(id, "pi_"+strings.Repeat("x", 60), at); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(blobs.m["rl-00"]); got > linkBucketMaxBytes {
		t.Fatalf("bucket %d B", got)
	}
	if _, ok := ls.find(ids[0]); ok {
		t.Fatal("oldest link should have been evicted")
	}
	if _, ok := ls.find(ids[len(ids)-1]); !ok {
		t.Fatal("newest link lost")
	}
}

func TestP4_LegacyKeysStillRefund(t *testing.T) {
	w := newWorld(t)
	for i := 1; i <= 1000; i++ {
		w.raw.m[fmt.Sprintf("sale_pi:%s", saleID(i))] = fmt.Sprintf("pi_old_%d", i)
	}
	api := &refundAPI{}
	if _, dec := w.refund(api, eventPayload{OriginalSaleID: saleID(7), Amount: 500}); dec != "" || api.refundedPIs()[0] != "pi_old_7" {
		t.Fatalf("legacy refund: dec=%q calls=%v", dec, api.calls)
	}
	w.sellMany(50)
	n := 0
	for k := range w.raw.m {
		if strings.HasPrefix(k, "sale_pi:") {
			n++
		}
	}
	if n != 1000 {
		t.Fatalf("legacy keys now %d, want exactly 1000 (no new sale_pi keys)", n)
	}
}

func TestP5_TillAtExactCap(t *testing.T) {
	w := newWorld(t)
	w.raw.m[lastTxnKey] = `{"outcome":"approved","auth_code":"pi_prev"}`
	for i := 0; len(w.raw.m) < hostMaxKeys; i++ {
		w.raw.m[fmt.Sprintf("sale_pi:fill-%d", i)] = "pi_x"
	}
	w.sellMany(5)
	api := &refundAPI{}
	if _, dec := w.refund(api, eventPayload{OriginalSaleID: saleID(5), Amount: 1005}); dec != "" || api.refundedPIs()[0] != "pi_5" {
		t.Fatalf("capped till: dec=%q calls=%v logs=%v", dec, api.calls, w.logs)
	}
	if len(w.raw.m) != hostMaxKeys {
		t.Fatalf("keys = %d", len(w.raw.m))
	}
	// last_txn absent AND no room: attempts + last_txn fall back to blobs.
	w2 := newWorld(t)
	for i := 0; len(w2.raw.m) < hostMaxKeys; i++ {
		w2.raw.m[fmt.Sprintf("sale_pi:fill-%d", i)] = "pi_x"
	}
	w2.sellMany(3)
	putAttempt(w2.kv, attemptEntry{ID: "req-1", Ref: "pi_a"})
	if e, ok := findAttempt(w2.kv, "req-1"); !ok || e.Ref != "pi_a" {
		t.Fatalf("attempt lost at the cap: %+v %v", e, ok)
	}
	api2 := &refundAPI{}
	if _, dec := w2.refund(api2, eventPayload{OriginalSaleID: saleID(3), Amount: 1003}); dec != "" || api2.refundedPIs()[0] != "pi_3" {
		t.Fatalf("capped till without last_txn: dec=%q logs=%v", dec, w2.logs)
	}
}

func TestP6_WriteFailuresLoggedNeverBlock(t *testing.T) {
	w := newWorld(t)
	w.raw.failAll, w.blobs.deny = true, true
	w.authorize("auth-1", "pi_1", 1000, 0)
	w.settle(eventPayload{SaleID: saleID(1), Amount: 1000})
	if len(w.logs) == 0 {
		t.Fatal("failures were swallowed silently")
	}
	api := &refundAPI{}
	if id, dec := w.refund(api, eventPayload{OriginalSaleID: saleID(1), Amount: 1000}); dec != declineUnknownOriginal || id != "" || len(api.calls) != 0 {
		t.Fatalf("id=%q dec=%q calls=%v", id, dec, api.calls)
	}
	// Storage fine, blob store failing: link not written, logged, refund declines.
	w2 := newWorld(t)
	w2.blobs.deny = true
	w2.authorize("auth-1", "pi_1", 1000, 0)
	w2.settle(eventPayload{SaleID: saleID(1), Amount: 1000})
	if !strings.Contains(strings.Join(w2.logs, "\n"), "not linked") {
		t.Fatalf("no 'not linked' log: %v", w2.logs)
	}
}

func TestP7_ReadGrowLargeValues(t *testing.T) {
	for _, size := range []int{100, 5000, 64 << 10} {
		val := strings.Repeat("v", size)
		got, n := readGrow(func(dst []byte) int32 {
			copy(dst, val)
			return int32(len(val))
		})
		if got != val || int(n) != size {
			t.Fatalf("size %d: got %d bytes (n=%d)", size, len(got), n)
		}
	}
	if got, n := readGrow(func([]byte) int32 { return -1 }); got != "" || n != -1 {
		t.Fatal("missing key must read empty")
	}
}

func TestP8_InterleavedSalesNeverCrossLink(t *testing.T) {
	// B authorizes before A settles; amounts differ -> each links to its own.
	w := newWorld(t)
	w.authorize("auth-A", "pi_A", 1000, 0)
	w.authorize("auth-B", "pi_B", 1200, 0)
	w.settle(eventPayload{SaleID: "sale-A", Amount: 1000})
	w.settle(eventPayload{SaleID: "sale-B", Amount: 1200})
	for sale, want := range map[string]string{"sale-A": "pi_A", "sale-B": "pi_B"} {
		api := &refundAPI{}
		w.refund(api, eventPayload{OriginalSaleID: sale, Amount: 100})
		if got := api.refundedPIs(); len(got) != 1 || got[0] != want {
			t.Fatalf("%s refunded %v, want %s", sale, got, want)
		}
	}
	// Same amounts: ambiguous -> neither linked, and no call is made on B's charge.
	w2 := newWorld(t)
	w2.authorize("auth-A", "pi_A", 1000, 0)
	w2.authorize("auth-B", "pi_B", 1000, 0)
	w2.settle(eventPayload{SaleID: "sale-A", Amount: 1000})
	w2.settle(eventPayload{SaleID: "sale-B", Amount: 1000})
	api := &refundAPI{}
	if _, dec := w2.refund(api, eventPayload{OriginalSaleID: "sale-A", Amount: 1000}); dec != declineUnknownOriginal || len(api.calls) != 0 {
		t.Fatalf("ambiguous refund: dec=%q calls=%v", dec, api.calls)
	}
	if !strings.Contains(strings.Join(w2.logs, "\n"), "ambiguous") {
		t.Fatalf("ambiguity not logged: %v", w2.logs)
	}
	// With authorize_request_id on settle both are exact, even at equal amounts.
	w3 := newWorld(t)
	w3.authorize("auth-A", "pi_A", 1000, 0)
	w3.authorize("auth-B", "pi_B", 1000, 0)
	w3.settle(eventPayload{SaleID: "sale-B", Amount: 1000, AuthorizeRequestID: "auth-B"})
	w3.settle(eventPayload{SaleID: "sale-A", Amount: 1000, AuthorizeRequestID: "auth-A"})
	for sale, want := range map[string]string{"sale-A": "pi_A", "sale-B": "pi_B"} {
		api := &refundAPI{}
		w3.refund(api, eventPayload{OriginalSaleID: sale, Amount: 1000})
		if got := api.refundedPIs(); len(got) != 1 || got[0] != want {
			t.Fatalf("%s refunded %v, want %s", sale, got, want)
		}
	}
}

func TestApprovalUsedOnceByAmount(t *testing.T) {
	w := newWorld(t)
	w.authorize("auth-A", "pi_A", 1000, 0)
	w.settle(eventPayload{SaleID: "sale-A", Amount: 1000})
	w.settle(eventPayload{SaleID: "sale-C", Amount: 1000}) // a sale paid another way
	api := &refundAPI{}
	if _, dec := w.refund(api, eventPayload{OriginalSaleID: "sale-C", Amount: 1000}); dec != declineUnknownOriginal || len(api.calls) != 0 {
		t.Fatalf("sale-C must not claim A's charge: dec=%q calls=%v", dec, api.calls)
	}
}

func TestP9_SplitTenderLegs(t *testing.T) {
	w := newWorld(t)
	legs := []origPayment{{TransactionRef: "pi_1", Amount: 500}, {TransactionRef: "pi_2", Amount: 2000}}
	api := &refundAPI{}
	if _, dec := w.refund(api, eventPayload{Amount: 1500, OriginalPayments: legs}); dec != "" || fmt.Sprint(api.refundedPIs()) != "[pi_2]" {
		t.Fatalf("1500 -> %v dec=%q", api.refundedPIs(), dec)
	}
	api = &refundAPI{answers: map[string]string{"pi_1": `{"error":{"code":"charge_already_refunded"}}`}}
	if _, dec := w.refund(api, eventPayload{Amount: 400, OriginalPayments: legs}); dec != "" || fmt.Sprint(api.refundedPIs()) != "[pi_1 pi_2]" {
		t.Fatalf("400 -> %v dec=%q", api.refundedPIs(), dec)
	}
	api = &refundAPI{answers: map[string]string{"pi_1": `{"error":{"code":"card_declined"}}`}}
	if _, dec := w.refund(api, eventPayload{Amount: 400, OriginalPayments: legs}); dec != "card_declined" || len(api.calls) != 1 {
		t.Fatalf("other error must decline without trying the next leg: dec=%q calls=%v", dec, api.calls)
	}
}

func TestP10_Reversal(t *testing.T) {
	w := newWorld(t)
	api := &refundAPI{}
	if _, dec := w.refund(api, eventPayload{Reversal: true, Amount: 900, OriginalPayments: []origPayment{{TransactionRef: "pi_h", Amount: 900}}}); dec != "" || api.refundedPIs()[0] != "pi_h" {
		t.Fatalf("host reversal: dec=%q calls=%v", dec, api.calls)
	}
	w.authorize("auth-9", "pi_9", 900, 0)
	api = &refundAPI{}
	if _, dec := w.refund(api, eventPayload{Reversal: true, Amount: 900, AuthorizeRequestID: "auth-9"}); dec != "" || api.refundedPIs()[0] != "pi_9" {
		t.Fatalf("current-host reversal: dec=%q calls=%v", dec, api.calls)
	}
	if r := loadRecent(w.kv); len(r) != 1 || !r[0].Used {
		t.Fatalf("reversed approval not marked used: %+v", r)
	}
	// A reversal naming no known approval never guesses.
	api = &refundAPI{}
	if _, dec := w.refund(api, eventPayload{Reversal: true, Amount: 900, AuthorizeRequestID: "auth-unknown"}); dec != declineUnknownOriginal || len(api.calls) != 0 {
		t.Fatalf("dec=%q calls=%v", dec, api.calls)
	}
}

func TestP11_RetryDoesNotDuplicateRecent(t *testing.T) {
	w := newWorld(t)
	w.authorize("auth-1", "pi_1", 1000, 0)
	w.authorize("auth-1", "pi_1", 1000, 0)
	if r := loadRecent(w.kv); len(r) != 1 {
		t.Fatalf("recent = %+v", r)
	}
	for i := 0; i < 20; i++ {
		w.authorize(fmt.Sprintf("a%d", i), "pi", 1, 0)
	}
	if r := loadRecent(w.kv); len(r) != maxRecent {
		t.Fatalf("recent len = %d, want %d", len(r), maxRecent)
	}
}

func TestP12_V130LastTxnReadable(t *testing.T) {
	w := newWorld(t)
	w.raw.m[lastTxnKey] = `{"provider":"stripe","amount":1000,"currency":"gbp","outcome":"approved","auth_code":"pi_old"}`
	if r := loadRecent(w.kv); len(r) != 0 {
		t.Fatalf("recent = %+v", r)
	}
	w.settle(eventPayload{SaleID: "sale-1", Amount: 1000}) // no recent: nothing linked, no panic
	w.authorize("auth-2", "pi_2", 1000, 0)
	var top map[string]any
	_ = json.Unmarshal([]byte(w.raw.m[lastTxnKey]), &top)
	if top["auth_code"] != "pi_2" || top["recent"] == nil {
		t.Fatalf("last_txn = %v", top)
	}
}

func TestOverflowKVUsesBlobWhenStorageRefuses(t *testing.T) {
	w := newWorld(t)
	for i := 0; len(w.raw.m) < hostMaxKeys; i++ {
		w.raw.m[fmt.Sprintf("k%d", i)] = "x"
	}
	if err := w.kv.put("attempts", []byte("[1]")); err != nil {
		t.Fatal(err)
	}
	if w.kv.get("attempts") != "[1]" {
		t.Fatal("blob fallback not read back")
	}
	w.blobs.deny = true
	if err := w.kv.put("other", []byte("x")); err == nil {
		t.Fatal("both stores refused but no error returned")
	}
}

func TestParseEventHostFields(t *testing.T) {
	ev := parseEvent([]byte(`{"id":"e1","type":"payment.stripe.refund","payload":{"amount":500,"original_sale_id":"s1","original_payments":[{"transaction_ref":"pi_9","amount":500,"tip_amount":0}]}}`))
	if ev.Payload.OriginalPayments[0].TransactionRef != "pi_9" || ev.Payload.Amount != 500 {
		t.Fatalf("%+v", ev)
	}
}

// ut-docs#6438 D1: after 1,100 sales and refunds no log line carries a full
// provider reference or sale id; only the masked tail ("...1100") may appear.
func TestP13_NoFullReferenceInLogs(t *testing.T) {
	w := newWorld(t)
	w.sellMany(1100)
	api := &refundAPI{}
	if _, dec := w.refund(api, eventPayload{OriginalSaleID: saleID(1100), Amount: 1000}); dec != "" {
		t.Fatalf("refund declined: %q", dec)
	}
	w.settle(eventPayload{SaleID: saleID(5000), Amount: 7}) // no match: logs the sale
	for _, l := range w.logs {
		for _, full := range []string{"pi_1100", "re_pi_1100", saleID(5000)} {
			if strings.Contains(l, full) {
				t.Fatalf("log leaks %q: %s", full, l)
			}
		}
	}
	if len(w.logs) == 0 {
		t.Fatal("expected the refund and unlinked-sale log lines")
	}
}
