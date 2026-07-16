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
//     and (in test mode) presents a simulated card, then polls to the result.
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

func saveTxn(v []byte) {
	kp, kl := ptrOf([]byte("last_txn"))
	vp, vl := ptrOf(v)
	storageSet(kp, kl, vp, vl)
}

// stripeCall performs one Stripe API request and returns the decoded response
// body. ok is false only on a host/transport failure (not an HTTP 4xx, which
// still returns a body describing the error).
func stripeCall(method, path, sk, form string) (body []byte, ok bool) {
	reqJSON, _ := json.Marshal(map[string]any{
		"method": method,
		"url":    apiBase + path,
		"headers": map[string]string{
			"Authorization": "Bearer " + sk,
			"Content-Type":  "application/x-www-form-urlencoded",
		},
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func main() {
	raw, _ := io.ReadAll(os.Stdin)
	var ev struct {
		Payload struct {
			Amount int64 `json:"amount"`
		} `json:"payload"`
	}
	_ = json.Unmarshal(raw, &ev)
	amount := ev.Payload.Amount

	sk := strings.TrimSpace(setting("stripe_secret_key"))
	if sk == "" {
		decline(amount, "", "no_secret_key")
	}
	currency := strings.TrimSpace(setting("currency"))
	if currency == "" {
		currency = "gbp"
	}

	if readerID := strings.TrimSpace(setting("stripe_reader_id")); readerID != "" {
		terminalCharge(sk, currency, readerID, amount)
	}
	onlineCharge(sk, currency, amount)
}

// terminalCharge drives a Stripe Terminal reader (card-present). In test mode
// (sk_test_) it presents a simulated card so the whole flow works with no
// hardware; with a live key + real reader the customer taps their card.
func terminalCharge(sk, currency, readerID string, amount int64) {
	// 1. Create a card_present PaymentIntent (auto-captured on success).
	form := fmt.Sprintf("amount=%d&currency=%s&payment_method_types[]=card_present&capture_method=automatic", amount, currency)
	body, ok := stripeCall("POST", "/v1/payment_intents", sk, form)
	if !ok {
		decline(amount, currency, "network_error")
	}
	var pi struct {
		ID    string `json:"id"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &pi)
	if pi.ID == "" {
		decline(amount, currency, firstNonEmpty(pi.Error.Code, "create_failed"))
	}

	// 2. Tell the reader to collect + process the payment.
	body, ok = stripeCall("POST", "/v1/terminal/readers/"+readerID+"/process_payment_intent", sk, "payment_intent="+pi.ID)
	if !ok {
		decline(amount, currency, "network_error")
	}
	var rd struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &rd)
	if rd.Error.Code != "" {
		logf("stripe: reader %s: %s", rd.Error.Code, rd.Error.Message)
		decline(amount, currency, firstNonEmpty(rd.Error.Code, "reader_error"))
	}

	// 3. Test mode: present a simulated card so the PI can complete.
	if strings.HasPrefix(sk, "sk_test_") {
		stripeCall("POST", "/v1/test_helpers/terminal/readers/"+readerID+"/present_payment_method", sk, "")
	}

	// 4. Poll the PaymentIntent to a terminal state. Up to ~60s so a live
	// customer has time to tap; the simulated reader completes at once.
	for i := 0; i < 60; i++ {
		body, ok = stripeCall("GET", "/v1/payment_intents/"+pi.ID, sk, "")
		if ok {
			var p struct {
				Status string `json:"status"`
			}
			_ = json.Unmarshal(body, &p)
			switch p.Status {
			case "succeeded", "requires_capture":
				approve(amount, currency, pi.ID)
			case "canceled":
				decline(amount, currency, "canceled")
			}
		}
		time.Sleep(time.Second)
	}
	decline(amount, currency, "reader_timeout")
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
