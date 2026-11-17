package main

// Host-facing plumbing, untagged so `go test` covers it (ut-docs#6438). main.go
// (wasip1 only) wires the real host functions to these interfaces; the tests
// wire fakes that enforce the REAL host limits (1,024 storage keys, 128 B key,
// 64 KiB value; blob names, quota).

import (
	"encoding/json"
	"fmt"
	"strings"
)

// kvStore is plugin storage. put reports the host's refusal (for example -4
// when a NEW key would be the 1,025th): callers log it, never ignore it.
type kvStore interface {
	get(k string) string
	put(k string, v []byte) error
}

// blobStore is the blob:own file store (wasm_blob.go in universal-till).
// get reports found=false for "no such blob" and an error for anything else.
type blobStore interface {
	get(name string) (data []byte, found bool, err error)
	put(name string, b []byte) error
}

// logHook is set by main.go to the host's log_write; tests capture it.
var logHook func(msg string)

func logf(format string, args ...any) {
	if logHook != nil {
		logHook(fmt.Sprintf(format, args...))
	}
}

// readGrow calls a host read (which fills dst and returns the FULL length of
// the value) with a 4 KiB buffer and, if the value is longer, once more with a
// buffer of exactly that length. Values reach 64 KiB (the storage cap), so a
// fixed 4 KiB buffer silently dropped any larger one (ut-docs#6438).
func readGrow(call func(dst []byte) int32) (string, int32) {
	buf := make([]byte, 4096)
	n := call(buf)
	if n < 0 {
		return "", n
	}
	if int(n) > len(buf) {
		buf = make([]byte, n)
		n = call(buf)
		if n < 0 {
			return "", n
		}
		if int(n) > len(buf) {
			return "", -1
		}
	}
	return string(buf[:n]), n
}

// overflowKV is the plugin's storage with a fallback: a till already at the
// host's 1,024-key cap cannot create ANY new key, so a write the storage
// refuses goes to the blob "kv-<key>" and reads look there when storage has
// nothing. It only ever carries the few fixed keys (last_txn, attempts).
type overflowKV struct {
	raw   kvStore
	blobs blobStore
}

func (o overflowKV) get(k string) string {
	if v := o.raw.get(k); v != "" {
		return v
	}
	if o.blobs == nil {
		return ""
	}
	b, found, err := o.blobs.get("kv-" + k)
	if err != nil || !found {
		return ""
	}
	return string(b)
}

func (o overflowKV) put(k string, v []byte) error {
	err := o.raw.put(k, v)
	if err == nil || o.blobs == nil {
		return err
	}
	if berr := o.blobs.put("kv-"+k, v); berr != nil {
		return fmt.Errorf("storage: %v; blob: %v", err, berr)
	}
	return nil
}

// origPayment is one leg of the original sale as core reports it on a refund
// (`original_payments`, ut-docs#6438 contract).
type origPayment struct {
	TransactionRef string `json:"transaction_ref"`
	Amount         int64  `json:"amount"`
	TipAmount      int64  `json:"tip_amount"`
}

type eventPayload struct {
	Amount             int64         `json:"amount"`
	SaleID             string        `json:"sale_id"`
	OriginalSaleID     string        `json:"original_sale_id"`
	Currency           string        `json:"currency"`
	Reversal           bool          `json:"reversal"`
	AuthorizeRequestID string        `json:"authorize_request_id"`
	TransactionRef     string        `json:"transaction_ref"`
	OriginalPayments   []origPayment `json:"original_payments"`
}

type event struct {
	ID      string       `json:"id"`
	Type    string       `json:"type"`
	Payload eventPayload `json:"payload"`
}

func parseEvent(raw []byte) event {
	var ev event
	_ = json.Unmarshal(raw, &ev)
	ev.Payload.TransactionRef = strings.TrimSpace(ev.Payload.TransactionRef)
	return ev
}

// maskRef shortens a provider reference for logs: the till log is shipped in
// diagnostics, and a full charge id has no business there (ut-docs#6438).
func maskRef(s string) string {
	if len(s) <= 6 {
		return "***"
	}
	return "..." + s[len(s)-4:]
}

// scrubRefs masks every id-like token (a provider id such as pi_..., re_...,
// a UUID or a long hex/alnum run) inside free text before it is logged, e.g. a
// provider error message that quotes the charge it refers to (ut-docs#6438).
func scrubRefs(s string) string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return !(r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	})
	for _, f := range fields {
		if len(f) >= 12 && strings.ContainsAny(f, "0123456789") {
			s = strings.ReplaceAll(s, f, maskRef(f))
		}
	}
	return s
}
