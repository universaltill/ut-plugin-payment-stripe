//go:build wasip1

// Stripe card payments — a WASI command (GOOS=wasip1 GOARCH=wasm) the till runs
// in-process for every payment.stripe.authorize event. Authorization is
// BLOCKING and runs BEFORE the sale completes: we ask Stripe to charge the
// amount and exit 0 (approved → the tender proceeds) or non-zero (declined →
// the till refuses the sale, basket intact).
//
// Two modes, chosen by settings:
//   - TERMINAL (card-present) when `stripe_reader_id` is set: drives a Stripe
//     Terminal reader (real, e.g. Reader S700 or WisePOS E (server-driven), or a test simulated
//     reader). Creates a card_present PaymentIntent, processes it on the reader,
//     and (in test mode) presents a simulated card, then polls to the result —
//     inside the till's 30 s payment gate, cancelling the reader and the
//     PaymentIntent before it closes (terminal.go, ut-docs#6304).
//   - ONLINE (no reader): charges a test payment method server-side — a demo
//     flow; amounts whose minor units end in 13 decline.
//
// Config (plugin settings): `stripe_secret_key` (sk_test_… / sk_live_…),
// `currency` (ISO, default gbp), and optional `stripe_reader_id` (tmr_…).
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unsafe"
)

//go:wasmimport ut log_write
func logWrite(ptr, n uint32)

//go:wasmimport ut settings_get
func settingsGet(kPtr, kLen, dstPtr, dstCap uint32) int32

//go:wasmimport ut view_query
func viewQuery(nPtr, nLen, aPtr, aLen, dstPtr, dstCap uint32) int32

//go:wasmimport ut http_request
func httpRequest(rPtr, rLen, dstPtr, dstCap uint32) int32

//go:wasmimport ut storage_set
func storageSet(kPtr, kLen, vPtr, vLen uint32) int32

//go:wasmimport ut storage_get
func storageGet(kPtr, kLen, dstPtr, dstCap uint32) int32

//go:wasmimport ut blob_put_open
func blobPutOpen(nPtr, nLen uint32) int32

//go:wasmimport ut blob_write
func blobWrite(h int32, ptr, n uint32) int32

//go:wasmimport ut blob_commit
func blobCommit(h int32) int32

//go:wasmimport ut blob_get_open
func blobGetOpen(nPtr, nLen uint32) int32

//go:wasmimport ut blob_read
func blobRead(h int32, dstPtr, dstCap uint32) int32

const apiBase = "https://api.stripe.com"

func ptrOf(b []byte) (uint32, uint32) {
	if len(b) == 0 {
		return 0, 0
	}
	return uint32(uintptr(unsafe.Pointer(&b[0]))), uint32(len(b))
}

func init() {
	logHook = func(m string) {
		p, n := ptrOf([]byte(m))
		logWrite(p, n)
	}
}

// setting reads one of the plugin's own settings. Grows the buffer once if the
// host reports a longer value than fits (writeGuest returns the full length).
func setting(key string) string {
	kp, kl := ptrOf([]byte(key))
	buf := make([]byte, 4096)
	bp, bc := ptrOf(buf)
	n := settingsGet(kp, kl, bp, bc)
	if n < 0 {
		return ""
	}
	if int(n) > len(buf) {
		buf = make([]byte, n)
		bp, bc = ptrOf(buf)
		n = settingsGet(kp, kl, bp, bc)
		if n < 0 || int(n) > len(buf) {
			return ""
		}
	}
	return string(buf[:n])
}

// shopContext reads the shop's currency from the core view shop.context.v2
// (view:shop). Known is false when the view is unavailable: resolveCurrency
// then relies on the sale's currency or declines, never guesses.
func shopContext() shopInfo {
	np, nl := ptrOf([]byte("shop.context.v2"))
	ap, al := ptrOf([]byte("{}"))
	buf := make([]byte, 8192)
	bp, bc := ptrOf(buf)
	n := viewQuery(np, nl, ap, al, bp, bc)
	if n >= 0 && int(n) > len(buf) {
		buf = make([]byte, n)
		bp, bc = ptrOf(buf)
		n = viewQuery(np, nl, ap, al, bp, bc)
	}
	if n < 0 || int(n) > len(buf) {
		return shopInfo{}
	}
	code, dec, ok := parseShopContext(buf[:n])
	return shopInfo{Code: code, Decimals: dec, Known: ok}
}

// hostStorage is the raw plugin storage; put reports the host's code.
type hostStorage struct{}

func (hostStorage) put(key string, v []byte) error {
	kp, kl := ptrOf([]byte(key))
	vp, vl := ptrOf(v)
	if rc := storageSet(kp, kl, vp, vl); rc != 0 {
		return fmt.Errorf("storage_set %q: host code %d", key, rc)
	}
	return nil
}

func (hostStorage) get(key string) string {
	kp, kl := ptrOf([]byte(key))
	v, _ := readGrow(func(dst []byte) int32 {
		dp, dc := ptrOf(dst)
		return storageGet(kp, kl, dp, dc)
	})
	return v
}

// hostBlobs is blob:own (ADR-0121). Codes: -1 no such blob, -2 permission not
// granted, -4 bad name, -5 quota.
type hostBlobs struct{}

const maxBlobRead = 1 << 20

func (hostBlobs) put(name string, b []byte) error {
	np, nl := ptrOf([]byte(name))
	h := blobPutOpen(np, nl)
	if h < 0 {
		return fmt.Errorf("blob_put_open %q: host code %d", name, h)
	}
	if len(b) > 0 {
		bp, bl := ptrOf(b)
		if n := blobWrite(h, bp, bl); n != int32(len(b)) {
			return fmt.Errorf("blob_write %q: host code %d", name, n)
		}
	}
	if rc := blobCommit(h); rc != 0 {
		return fmt.Errorf("blob_commit %q: host code %d", name, rc)
	}
	return nil
}

func (hostBlobs) get(name string) ([]byte, bool, error) {
	np, nl := ptrOf([]byte(name))
	h := blobGetOpen(np, nl)
	if h == -1 {
		return nil, false, nil
	}
	if h < 0 {
		return nil, false, fmt.Errorf("blob_get_open %q: host code %d", name, h)
	}
	var out []byte
	buf := make([]byte, 16<<10)
	for {
		bp, bc := ptrOf(buf)
		n := blobRead(h, bp, bc)
		if n < 0 {
			return nil, false, fmt.Errorf("blob_read %q: host code %d", name, n)
		}
		if n == 0 {
			return out, true, nil // the host releases the handle at the end
		}
		out = append(out, buf[:n]...)
		if len(out) > maxBlobRead {
			return nil, false, fmt.Errorf("blob %q larger than %d bytes", name, maxBlobRead)
		}
	}
}

var (
	pluginKV    kvStore = overflowKV{raw: hostStorage{}, blobs: hostBlobs{}}
	pluginLinks         = linkStore{blobs: hostBlobs{}}
)

// persistResult is false for refund and settle events: only an authorize
// answer is recorded in last_txn. curEventID is the authorize event's id.
var (
	persistResult = true
	curEventID    string
)

// stripeCall performs one Stripe API request and returns the decoded response
// body. ok is false only on a host/transport failure (not an HTTP 4xx, which
// still returns a body describing the error).
func stripeCall(method, path, sk, form string) (body []byte, status int, ok bool) {
	return stripeCallIdem(method, path, sk, form, "")
}

// stripeCallIdem is stripeCall with an optional Idempotency-Key header.
func stripeCallIdem(method, path, sk, form, idemKey string) (body []byte, status int, ok bool) {
	headers := map[string]string{
		"Authorization": "Bearer " + sk,
		"Content-Type":  "application/x-www-form-urlencoded",
	}
	if idemKey != "" {
		headers["Idempotency-Key"] = idemKey
	}
	reqJSON, _ := json.Marshal(map[string]any{
		"method":   method,
		"url":      apiBase + path,
		"headers":  headers,
		"body_b64": base64.StdEncoding.EncodeToString([]byte(form)),
	})
	rp, rl := ptrOf(reqJSON)
	respBuf := make([]byte, 64*1024)
	bp, bc := ptrOf(respBuf)
	code := httpRequest(rp, rl, bp, bc)
	if code < 0 || int(code) > len(respBuf) {
		return nil, 0, false
	}
	var httpResp struct {
		Status  int    `json:"status"`
		BodyB64 string `json:"body_b64"`
	}
	_ = json.Unmarshal(respBuf[:code], &httpResp)
	b, _ := base64.StdEncoding.DecodeString(httpResp.BodyB64)
	return b, httpResp.Status, true
}

const (
	approved = 0
	declined = 2 // non-zero exit → the till declines the tender (basket kept)
)

func approve(amount int64, currency, authCode string) {
	fields := map[string]any{
		"provider": "stripe", "amount": amount, "currency": currency,
		"outcome": "approved", "auth_code": authCode,
	}
	if persistResult {
		// transaction_ref: core stores it with the sale and hands it back on a
		// refund (ut-docs#6438); auth_code keeps its meaning.
		fields["transaction_ref"] = authCode
		saveAuthorizeResult(pluginKV, fields, &recentEntry{
			ID: curEventID, Ref: authCode, Amount: amount, At: time.Now().Unix(),
		})
	}
	result, _ := json.Marshal(fields)
	logf("stripe: APPROVED %d %s (%s)", amount, currency, maskRef(authCode))
	_, _ = os.Stdout.Write(append(result, '\n'))
	os.Exit(approved)
}

func decline(amount int64, currency, reason string) {
	fields := declineFields(amount, currency, reason)
	if persistResult {
		saveAuthorizeResult(pluginKV, fields, nil)
	}
	result := declineAnswer(amount, currency, reason)
	logf("stripe: DECLINED %d %s (%s)", amount, currency, scrubRefs(reason))
	_, _ = os.Stdout.Write(append(result, '\n'))
	os.Exit(declined)
}

// hostStripe and realClock wire terminal.go's flow to the host.
type hostStripe struct{ sk string }

func (h hostStripe) call(method, path, form, idemKey string) ([]byte, int, bool) {
	return stripeCallIdem(method, path, h.sk, form, idemKey)
}

type realClock struct{}

func (realClock) now() time.Time        { return time.Now() }
func (realClock) sleep(d time.Duration) { time.Sleep(d) }

func main() {
	start := time.Now() // terminal.go's deadlines count from here (ut-docs#6304)
	raw, _ := io.ReadAll(os.Stdin)
	ev := parseEvent(raw)
	amount := ev.Payload.Amount
	curEventID = ev.ID

	// Post-sale settle notification: when core already holds the reference
	// (transaction_ref) nothing is stored; otherwise link the sale to its
	// approval in a link bucket (links.go, recent.go). Never blocks the sale.
	if strings.HasSuffix(ev.Type, ".requested") {
		settleSale(pluginKV, pluginLinks, ev.Payload, time.Now())
		os.Exit(0)
	}
	// Tap to Pay on iPhone entry (stripe_tap): no Stripe key, everything goes
	// through core's broker. Must run before the no_secret_key decline below.
	if isTapEvent(ev.Type) {
		tapMain(ev)
	}
	persistResult = !strings.HasSuffix(ev.Type, ".refund")

	// Reader state for the till's picker (ut-docs#6566): read-only, always
	// exit 0 (core reads "unknown" for anything it cannot parse).
	if strings.HasSuffix(ev.Type, ".readers") {
		persistResult = false
		readersHook(ev.Payload)
	}

	sk := strings.TrimSpace(setting("stripe_secret_key"))
	if sk == "" {
		decline(amount, "", "no_secret_key")
	}

	// Provider refund (blocking, pre-return): send the money back on the
	// original charge. Exit 0 only when Stripe accepts the refund. Refunds
	// carry no currency of their own, so no currency check here.
	if strings.HasSuffix(ev.Type, ".refund") {
		refundCharge(sk, ev.Payload)
	}

	// Currency (ut-docs#6604): saved choice ("auto" = the sale's, else the
	// shop's), declined when it does not match the sale.
	savedCurrency := setting("currency")
	shop := shopContext()
	currency, currencyDecline := resolveCurrency(savedCurrency, ev.Payload.Currency, shop)
	if currencyDecline != "" {
		decline(amount, declinedCurrency(savedCurrency, ev.Payload.Currency, shop), currencyDecline)
	}

	// Reader (ut-docs#6566): the till's per-call reader_id wins; a missing one
	// falls back to the setting. A malformed one declines, no fallback.
	readerID, readerDecline := chooseReader(ev.Payload.ReaderID, setting("stripe_reader_id"))
	if readerDecline != "" {
		decline(amount, "", readerDecline)
	}
	if readerID != "" {
		c := &terminalCharger{
			api: hostStripe{sk: sk}, clk: realClock{}, kv: pluginKV,
			sk: sk, currency: currency, readerID: readerID,
			requestID: ev.ID, amount: amount, start: start,
		}
		if out := c.run(); out.approved {
			approve(amount, currency, out.authCode)
		} else {
			decline(amount, currency, out.reason)
		}
	}
	onlineCharge(sk, currency, amount)
}

// readersHook answers payment.stripe.readers and exits 0. Without a secret key
// every reader is "unknown" (invalid_secret_key reason).
func readersHook(p eventPayload) {
	sk := strings.TrimSpace(setting("stripe_secret_key"))
	var out []byte
	if sk == "" {
		rows := []readerState{}
		if p.Op == "state" {
			for i, r := range p.Readers {
				if i >= maxStateReaders {
					break
				}
				rows = append(rows, readerState{ReaderID: r, State: "unknown", Reason: "no_secret_key"})
			}
		}
		out, _ = json.Marshal(map[string]any{"readers": rows})
	} else {
		out = readersAnswer(hostStripe{sk: sk}, p)
	}
	_, _ = os.Stdout.Write(append(out, '\n'))
	os.Exit(approved)
}

// refundCharge refunds (part of) the original sale's PaymentIntent, found via
// core's original_payments, the link bucket or the legacy key (refund.go).
// Without a reference it declines: refunding an unknown charge is impossible
// and guessing one would refund the wrong customer.
func refundCharge(sk string, p eventPayload) {
	id, reason := refundSale(hostStripe{sk: sk}, pluginKV, pluginLinks, p)
	if reason != "" {
		decline(p.Amount, "", reason)
	}
	approve(p.Amount, "", id)
}

// onlineCharge is the no-reader demo flow: charge a test payment method
// server-side. Deterministic in test mode — an amount whose minor units end in
// 13 declines.
func onlineCharge(sk, currency string, amount int64) {
	paymentMethod := "pm_card_visa"
	if amount%100 == 13 {
		paymentMethod = "pm_card_chargeDeclined"
	}
	form := fmt.Sprintf(
		"amount=%d&currency=%s&payment_method=%s&confirm=true&automatic_payment_methods[enabled]=true&automatic_payment_methods[allow_redirects]=never",
		amount, currency, paymentMethod)
	body, status, ok := stripeCall("POST", "/v1/payment_intents", sk, form)
	if id, reason := onlineDecision(status, ok, body); reason == "" {
		approve(amount, currency, id)
	} else {
		decline(amount, currency, reason)
	}
}
