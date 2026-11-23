//go:build wasip1

package main

// Host seam of the stripe_tap entry (ut-docs#6456). Both calls use the raw
// buffer ABI: min(len, dstCap) bytes written, the full length returned.
// tap_to_pay_request starts a card sheet, so it is NEVER repeated to grow the
// buffer: one 64 KiB buffer, an answer longer than that is "unknown" (pending).

import (
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"time"
)

//go:wasmimport ut payment_provider_call
func paymentProviderCall(pPtr, pLen, oPtr, oLen, rPtr, rLen, dstPtr, dstCap uint32) int32

//go:wasmimport ut tap_to_pay_request
func tapToPayRequest(rPtr, rLen, dstPtr, dstCap uint32) int32

const tapBufSize = 64 * 1024

type hostBroker struct{}

func (hostBroker) providerCall(provider, op string, body []byte) ([]byte, int32) {
	prov, o := []byte(provider), []byte(op)
	pp, pl := ptrOf(prov)
	op2, ol := ptrOf(o)
	rp, rl := ptrOf(body)
	buf := make([]byte, tapBufSize)
	bp, bc := ptrOf(buf)
	n := paymentProviderCall(pp, pl, op2, ol, rp, rl, bp, bc)
	runtime.KeepAlive(prov)
	runtime.KeepAlive(o)
	runtime.KeepAlive(body)
	if n < 0 {
		return nil, n
	}
	if int(n) > len(buf) {
		return nil, -3
	}
	return buf[:n], n
}

func (hostBroker) tapRequest() ([]byte, int32) {
	req := []byte("{}") // core ignores it: amount, currency, key come from the binding
	rp, rl := ptrOf(req)
	buf := make([]byte, tapBufSize)
	bp, bc := ptrOf(buf)
	n := tapToPayRequest(rp, rl, bp, bc)
	runtime.KeepAlive(req)
	if n < 0 {
		return nil, n
	}
	if int(n) > len(buf) {
		return nil, -3
	}
	return buf[:n], n
}

// tapMain handles every payment.stripe_tap.* event and exits. It runs BEFORE
// main()'s secret-key check: this entry has no Stripe key.
func tapMain(ev event) {
	amount, currency := ev.Payload.Amount, ev.Payload.Currency
	switch {
	case strings.HasSuffix(ev.Type, ".readers"):
		persistResult = false
		rows := []readerState{}
		if ev.Payload.Op == "state" {
			for i, r := range ev.Payload.Readers {
				if i >= maxStateReaders {
					break
				}
				rows = append(rows, readerState{ReaderID: r, State: "unknown", Reason: "not_managed"})
			}
		}
		out, _ := json.Marshal(map[string]any{"readers": rows})
		_, _ = os.Stdout.Write(append(out, '\n'))
		os.Exit(approved)
	case strings.HasSuffix(ev.Type, ".refund"):
		persistResult = false
		id, reason := tapRefund(hostBroker{}, ev.Payload)
		if reason != "" {
			decline(amount, "", reason)
		}
		approve(amount, "", id)
	case strings.HasSuffix(ev.Type, ".authorize"):
		out := tapAuthorize(hostBroker{}, pluginKV, tapRequest{Key: ev.ID, Amount: amount, Currency: currency}, time.Now().Unix())
		switch out.kind {
		case tapApproved:
			approve(amount, currency, out.ref)
		default:
			decline(amount, currency, out.code)
		}
	}
	decline(amount, currency, "unsupported_event")
}
