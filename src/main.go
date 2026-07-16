//go:build wasip1

// Stripe card payments — a WASI command (GOOS=wasip1 GOARCH=wasm) the till runs
// in-process for every payment.stripe.authorize event. Authorization is
// BLOCKING and runs BEFORE the sale completes: we ask Stripe to charge the
// amount and exit 0 (approved → the tender proceeds) or non-zero (declined →
// the till refuses the sale, basket intact).
//
// Config (plugin settings): `stripe_secret_key` (sk_test_… / sk_live_…) and
// `currency` (ISO code, default gbp). Nothing is hardcoded — one plugin serves
// every merchant, each with their own key.
//
// Test-mode outcomes are deterministic, like a demo terminal: an amount whose
// minor units end in 13 uses Stripe's decline test card; everything else uses
// the approve test card. (A real card reader would replace the test
// payment_method with a live one — Stripe Terminal.)
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
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

const (
	approved = 0
	declined = 2 // non-zero exit → the till declines the tender (basket kept)
)

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
		logf("stripe: no secret key configured — declining")
		os.Exit(declined)
	}
	currency := strings.TrimSpace(setting("currency"))
	if currency == "" {
		currency = "gbp"
	}

	// Deterministic test outcomes; a real reader supplies the live payment_method.
	paymentMethod := "pm_card_visa"
	if amount%100 == 13 {
		paymentMethod = "pm_card_chargeDeclined"
	}

	form := fmt.Sprintf(
		"amount=%d&currency=%s&payment_method=%s&confirm=true&automatic_payment_methods[enabled]=true&automatic_payment_methods[allow_redirects]=never",
		amount, currency, paymentMethod)
	reqJSON, _ := json.Marshal(map[string]any{
		"method": "POST",
		"url":    "https://api.stripe.com/v1/payment_intents",
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
	if code < 0 {
		logf("stripe: request failed (host code %d) — declining", code)
		os.Exit(declined)
	}
	if int(code) > len(respBuf) {
		logf("stripe: response too large (%d) — declining", code)
		os.Exit(declined)
	}

	var httpResp struct {
		Status  int    `json:"status"`
		BodyB64 string `json:"body_b64"`
	}
	_ = json.Unmarshal(respBuf[:code], &httpResp)
	body, _ := base64.StdEncoding.DecodeString(httpResp.BodyB64)

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
		result, _ := json.Marshal(map[string]any{
			"provider": "stripe", "amount": amount, "currency": currency,
			"outcome": "approved", "auth_code": pi.ID,
		})
		saveTxn(result)
		logf("stripe: APPROVED %d %s (%s)", amount, currency, pi.ID)
		_, _ = os.Stdout.Write(append(result, '\n'))
		return
	}

	reason := pi.Error.DeclineCode
	if reason == "" {
		reason = pi.Error.Code
	}
	result, _ := json.Marshal(map[string]any{
		"provider": "stripe", "amount": amount, "currency": currency,
		"outcome": "declined", "decline_code": reason,
	})
	saveTxn(result)
	logf("stripe: DECLINED %d %s (%s: %s)", amount, currency, reason, pi.Error.Message)
	_, _ = os.Stdout.Write(append(result, '\n'))
	os.Exit(declined)
}
