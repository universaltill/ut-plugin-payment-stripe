package main

// Per-call reader (ut-docs#6566). The till's reader registry may name the
// Stripe Terminal reader for one authorize call (`reader_id` in the payload),
// and may ask for the state of several readers (`payment.stripe.readers`).
// Untagged so `go test` covers it; main.go only wires the host.
//
// Security: the reader id is put into a Stripe URL path, so a payload id is
// format-checked first (an id such as `tmr_1/../x` must never reach a URL).

import (
	"encoding/json"
	"net/url"
	"strings"
)

const declineInvalidReader = "invalid_reader"

// maxStateReaders caps one state call: the check is sequential HTTP inside one
// guest call, and the host gate is short.
const maxStateReaders = 16

// validReaderID is Stripe Terminal's `tmr_` plus 1..64 letters or digits.
func validReaderID(id string) bool {
	if !strings.HasPrefix(id, "tmr_") {
		return false
	}
	rest := id[len("tmr_"):]
	if len(rest) < 1 || len(rest) > 64 {
		return false
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

// chooseReader picks the reader for an authorize call. A payload id wins; a
// missing or empty one falls back to the `stripe_reader_id` setting exactly as
// before (an old till never sends it). A payload id that is present but
// malformed declines with invalid_reader: never a fallback to another reader,
// which would charge a different physical device.
func chooseReader(payloadID, settingID string) (id, reason string) {
	if strings.TrimSpace(payloadID) == "" {
		return strings.TrimSpace(settingID), ""
	}
	if !validReaderID(payloadID) { // strict: no trimming of a non-empty id
		return "", declineInvalidReader
	}
	return payloadID, ""
}

type readerState struct {
	ReaderID string `json:"reader_id"`
	State    string `json:"state"` // ready | busy | offline | unknown
	Reason   string `json:"reason,omitempty"`
}

// readerStates asks Stripe for each reader's state (read-only GET). A ref that
// is not a valid reader id is never sent. Anything unclear is "unknown": the
// till then still lets the cashier choose, and Stripe fails safe on the charge.
func readerStates(api stripeAPI, refs []string) []readerState {
	if len(refs) > maxStateReaders {
		refs = refs[:maxStateReaders]
	}
	out := make([]readerState, 0, len(refs))
	for _, ref := range refs {
		if !validReaderID(ref) {
			out = append(out, readerState{ReaderID: ref, State: "unknown", Reason: declineInvalidReader})
			continue
		}
		out = append(out, oneReaderState(api, ref))
	}
	return out
}

func oneReaderState(api stripeAPI, ref string) readerState {
	rs := readerState{ReaderID: ref, State: "unknown"}
	body, status, ok := api.call("GET", "/v1/terminal/readers/"+url.PathEscape(ref), "", "")
	if !ok {
		logf("stripe: reader %s state: network error", maskRef(ref))
		return rs
	}
	switch status {
	case 404:
		rs.State, rs.Reason = "offline", "reader_unreachable"
		return rs
	case 401:
		rs.Reason = declineInvalidKey // never log Stripe's message: it echoes the key
		return rs
	case 200:
	default:
		return rs
	}
	var r struct {
		Status string          `json:"status"`
		Action json.RawMessage `json:"action"`
	}
	if json.Unmarshal(body, &r) != nil {
		return rs
	}
	hasAction := len(r.Action) > 0 && string(r.Action) != "null"
	switch r.Status {
	case "offline":
		rs.State = "offline"
	case "online":
		if hasAction {
			rs.State = "busy"
		} else {
			rs.State = "ready"
		}
	}
	return rs
}

// readersAnswer is the stdout JSON of the payment.stripe.readers hook. Only
// op "state" is served; anything else answers an empty list.
func readersAnswer(api stripeAPI, p eventPayload) []byte {
	rows := []readerState{}
	if p.Op == "state" {
		rows = readerStates(api, p.Readers)
	}
	b, _ := json.Marshal(map[string]any{"readers": rows})
	return b
}
