//go:build wasip1

// Stripe card payments — a WASI command (GOOS=wasip1 GOARCH=wasm) the till runs
// in-process for every payment.stripe.authorize event. Authorization is
// BLOCKING and runs BEFORE the sale completes: we ask Stripe to charge the
// amount and exit 0 (approved → the tender proceeds) or non-zero (declined →
// the till refuses the sale, basket intact).
//
// Two modes, chosen by settings:
//   - TERMINAL (card-present) when `stripe_reader_id` is set: drives a Stripe
//     Terminal reader (real, e.g. Reader S700/WisePOS E/M2, or a test simulated
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

//go:wasmimport ut http_request
func httpRequest(rPtr, rLen, dstPtr, dstCap uint32) int32

//go:wasmimport ut storage_set
func storageSet(kPtr, kLen, vPtr, vLen uint32) int32

//go:wasmimport ut storage_get
func storageGet(kPtr, kLen, dstPtr, dstCap uint32) int32

const apiBase = "https://api.stripe.com"

func ptrOf(b []byte) (uint32, uint32) {
	if len(b) == 0 {
		return 0, 0
	}
	return uint32(uintptr(unsafe.Pointer(&b[0]))), uint32(len(b))
}

func logf(format string, args ...any) {
	msg := []byte(fmt.Sprintf(format, args...))
	p, n := ptrOf(msg)
	logWrite(p, n)
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

func storagePut(key string, v []byte) {
	kp, kl := ptrOf([]byte(key))
	vp, vl := ptrOf(v)
	storageSet(kp, kl, vp, vl)
}

func storageRead(key string) string {
	kp, kl := ptrOf([]byte(key))
	buf := make([]byte, 4096)
	bp, bc := ptrOf(buf)
	n := storageGet(kp, kl, bp, bc)
	if n < 0 || int(n) > len(buf) {
		return ""
	}
	return string(buf[:n])
}

func saveTxn(v []byte) { storagePut("last_txn", v) }

// stripeCall performs one Stripe API request and returns the decoded response
// body. ok is false only on a host/transport failure (not an HTTP 4xx, which
// still returns a body describing the error).
func stripeCall(method, path, sk, form string) (body []byte, ok bool) {
	return stripeCallIdem(method, path, sk, form, "")
}

// stripeCallIdem is stripeCall with an optional Idempotency-Key header.
func stripeCallIdem(method, path, sk, form, idemKey string) (body []byte, ok bool) {
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
		return nil, false
	}
	var httpResp struct {
		BodyB64 string `json:"body_b64"`
	}
	_ = json.Unmarshal(respBuf[:code], &httpResp)
	b, _ := base64.StdEncoding.DecodeString(httpResp.BodyB64)
	return b, true
}

const (
	approved = 0
	declined = 2 // non-zero exit → the till declines the tender (basket kept)
)

func approve(amount int64, currency, authCode string) {
	result, _ := json.Marshal(map[string]any{
		"provider": "stripe", "amount": amount, "currency": currency,
		"outcome": "approved", "auth_code": authCode,
	})
	saveTxn(result)
	logf("stripe: APPROVED %d %s (%s)", amount, currency, authCode)
	_, _ = os.Stdout.Write(append(result, '\n'))
	os.Exit(approved)
}

func decline(amount int64, currency, reason string) {
	result, _ := json.Marshal(map[string]any{
		"provider": "stripe", "amount": amount, "currency": currency,
		"outcome": "declined", "decline_code": reason,
	})
	saveTxn(result)
	logf("stripe: DECLINED %d %s (%s)", amount, currency, reason)
	_, _ = os.Stdout.Write(append(result, '\n'))
	os.Exit(declined)
}

// hostStripe, realClock and pluginKV wire terminal.go's flow to the host.
type hostStripe struct{ sk string }

func (h hostStripe) call(method, path, form, idemKey string) ([]byte, bool) {
	return stripeCallIdem(method, path, h.sk, form, idemKey)
}

type realClock struct{}

func (realClock) now() time.Time        { return time.Now() }
func (realClock) sleep(d time.Duration) { time.Sleep(d) }

type pluginKV struct{}

func (pluginKV) get(k string) string    { return storageRead(k) }
func (pluginKV) put(k string, v []byte) { storagePut(k, v) }

func main() {
	start := time.Now() // terminal.go's deadlines count from here (ut-docs#6304)
	raw, _ := io.ReadAll(os.Stdin)
	var ev struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Payload struct {
			Amount         int64  `json:"amount"`
			SaleID         string `json:"sale_id"`
			OriginalSaleID string `json:"original_sale_id"`
			Currency       string `json:"currency"`
		} `json:"payload"`
	}
	_ = json.Unmarshal(raw, &ev)
	amount := ev.Payload.Amount

	// Post-sale settle notification: link the sale id to the PaymentIntent we
	// just authorized so a later refund can find the charge. Not blocking.
	if strings.HasSuffix(ev.Type, ".requested") {
		var last struct {
			AuthCode string `json:"auth_code"`
		}
		_ = json.Unmarshal([]byte(storageRead("last_txn")), &last)
		if ev.Payload.SaleID != "" && last.AuthCode != "" {
			storagePut("sale_pi:"+ev.Payload.SaleID, []byte(last.AuthCode))
			logf("stripe: sale %s settled with %s", ev.Payload.SaleID, last.AuthCode)
		}
		os.Exit(0)
	}

	sk := strings.TrimSpace(setting("stripe_secret_key"))
	if sk == "" {
		decline(amount, "", "no_secret_key")
	}
	currency := strings.TrimSpace(setting("currency"))
	if currency == "" {
		currency = "gbp"
	}

	// Provider refund (blocking, pre-return): send the money back on the
	// original charge. Exit 0 only when Stripe accepts the refund.
	if strings.HasSuffix(ev.Type, ".refund") {
		refundCharge(sk, ev.Payload.OriginalSaleID, amount)
	}

	if readerID := strings.TrimSpace(setting("stripe_reader_id")); readerID != "" {
		c := &terminalCharger{
			api: hostStripe{sk: sk}, clk: realClock{}, kv: pluginKV{},
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

// refundCharge refunds (part of) the original sale's PaymentIntent. The PI id
// was stored at settle time under sale_pi:<sale_id>; without it we must
// decline — refunding an unknown charge is not possible.
func refundCharge(sk, originalSaleID string, amount int64) {
	pi := strings.TrimSpace(storageRead("sale_pi:" + originalSaleID))
	if originalSaleID == "" || pi == "" {
		decline(amount, "", "unknown_original_charge")
	}
	form := fmt.Sprintf("payment_intent=%s&amount=%d", pi, amount)
	body, ok := stripeCall("POST", "/v1/refunds", sk, form)
	if !ok {
		decline(amount, "", "network_error")
	}
	var rf struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &rf)
	switch rf.Status {
	case "succeeded", "pending":
		logf("stripe: REFUNDED %d on %s (%s, %s)", amount, pi, rf.ID, rf.Status)
		approve(amount, "", rf.ID)
	}
	decline(amount, "", firstNonEmpty(rf.Error.Code, "refund_failed"))
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
	body, ok := stripeCall("POST", "/v1/payment_intents", sk, form)
	if !ok {
		decline(amount, currency, "network_error")
	}
	var pi struct {
		Status string `json:"status"`
		ID     string `json:"id"`
		Error  struct {
			Code        string `json:"code"`
			DeclineCode string `json:"decline_code"`
			Message     string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &pi)
	if pi.Status == "succeeded" {
		approve(amount, currency, pi.ID)
	}
	decline(amount, currency, firstNonEmpty(pi.Error.DeclineCode, pi.Error.Code, "declined"))
}
