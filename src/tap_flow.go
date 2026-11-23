package main

// Tap to Pay on iPhone entry (stripe_tap, ut-docs#6456, ADR-0198 3.6/3.7/3.10).
// Pure logic, untagged so `go test` covers it; tap.go (wasip1) wires the real
// host functions. The entry holds NO Stripe key and makes no HTTP call: it
// can only act through core's broker inside a dispatch core bound (amount,
// currency, attempt key and PaymentIntent all come from core, never from this
// plugin). Core confirms every answer with the broker itself, so this code can
// only tighten an outcome, never loosen it.

import (
	"encoding/json"
	"strings"
)

const (
	tapAttemptsKey = "tap_attempts" // ONE bounded key, last maxTapAttempts (host cap 1,024 keys)
	maxTapAttempts = 8

	declineStartFailed = "start_failed"    // nothing was charged (core: tapStartFailedError)
	declineOutcomeUnk  = "outcome_unknown" // core maps it to the pending screen
	declineCancelled   = "cancelled"
	declineRefundFail  = "refund_failed"
	declineRefundDeny  = "refund_denied"
)

// brokerAPI is the plugin's two routes to core: payment_provider_call and
// tap_to_pay_request. A negative rc is a host code (-2 not permitted / no
// bound dispatch, -3 internal, -4 bad args); otherwise the body is the answer.
type brokerAPI interface {
	providerCall(provider, op string, body []byte) ([]byte, int32)
	tapRequest() ([]byte, int32)
}

type tapOutcomeKind int

const (
	tapApproved tapOutcomeKind = iota + 1
	tapDeclined
	tapPending
)

type tapOutcome struct {
	kind tapOutcomeKind
	ref  string // approved: the PaymentIntent id
	code string // declined: decline_code
}

type tapRequest struct {
	Key      string // the gate's attempt key (event id)
	Amount   int64
	Currency string
}

// tapResult is devicepay.Result as tap_to_pay_request answers it (advisory).
type tapResult struct {
	Status        string `json:"status"`
	ProviderTxnID string `json:"provider_txn_id"`
	ChargeID      string `json:"charge_id"`
	CardBrand     string `json:"card_brand"`
	Last4         string `json:"last4"`
	DeclineCode   string `json:"decline_code"`
	Reason        string `json:"reason"`
	Error         struct {
		Code string `json:"code"`
	} `json:"error"`
}

// brokerStatus is the neutral session envelope (session_status / cancel).
type brokerStatus struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	Amount         int64  `json:"amount_minor"`
	AmountReceived int64  `json:"amount_received_minor"`
	Currency       string `json:"currency"`
	Error          struct {
		Code string `json:"code"`
	} `json:"error"`
}

// brokerErrorCode is the code of a broker failure answer, "" for none.
func brokerErrorCode(raw []byte) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return ""
	}
	return e.Error.Code
}

func isTapEvent(eventType string) bool { return strings.Contains(eventType, ".stripe_tap.") }

// startFailedCodes are the broker refusals that prove nothing was charged.
var startFailedCodes = map[string]bool{
	"not_onboarded": true, "not_configured": true, "disabled": true, "provider_unavailable": true,
	"invalid_amount": true, "currency_mismatch": true, "currency_unsupported": true,
	"device_role_forbidden": true, "device_role_unrecorded": true, "insecure_endpoint": true,
}

// ---- attempt records: one bounded key ----

type tapAttempt struct {
	ID       string `json:"id"`
	State    string `json:"state"` // pending | approved | declined
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	PI       string `json:"pi,omitempty"`
	At       int64  `json:"at"`
}

func loadTapAttempts(kv kvStore) []tapAttempt {
	var list []tapAttempt
	_ = json.Unmarshal([]byte(kv.get(tapAttemptsKey)), &list)
	return list
}

func findTapAttempt(kv kvStore, id string) (tapAttempt, bool) {
	for _, e := range loadTapAttempts(kv) {
		if e.ID == id && id != "" {
			return e, true
		}
	}
	return tapAttempt{}, false
}

// putTapAttempt stores e (replacing its id), newest last, last 8 kept. The
// error is the host's refusal: the caller must not tap without a record.
func putTapAttempt(kv kvStore, e tapAttempt) error {
	list := make([]tapAttempt, 0, maxTapAttempts)
	for _, old := range loadTapAttempts(kv) {
		if old.ID != e.ID {
			list = append(list, old)
		}
	}
	list = append(list, e)
	if len(list) > maxTapAttempts {
		list = list[len(list)-maxTapAttempts:]
	}
	b, _ := json.Marshal(list)
	return kv.put(tapAttemptsKey, b)
}

func pendingOut() tapOutcome { return tapOutcome{kind: tapPending, code: declineOutcomeUnk} }
func startFailedOut() tapOutcome {
	return tapOutcome{kind: tapDeclined, code: declineStartFailed}
}

// tapAuthorize runs one authorize for the bound dispatch. now is unix seconds.
func tapAuthorize(b brokerAPI, kv kvStore, r tapRequest, now int64) tapOutcome {
	if r.Key == "" {
		logf("stripe_tap: no attempt key, nothing started")
		return startFailedOut()
	}
	rec, had := findTapAttempt(kv, r.Key)
	// A declined record means nothing was charged: tap again. Anything else
	// may have a PaymentIntent behind it, so ask the broker first (3.6).
	if had && rec.State != "declined" {
		raw, rc := b.providerCall("stripe", "session_status", []byte("{}"))
		if rc < 0 {
			logf("stripe_tap: status lookup host code %d", rc)
			return pendingOut()
		}
		var st brokerStatus
		if json.Unmarshal(raw, &st) != nil || st.Error.Code != "" {
			logf("stripe_tap: status lookup failed (%s)", scrubRefs(st.Error.Code))
			return pendingOut()
		}
		switch st.Status {
		case "succeeded":
			if st.ID == "" || st.AmountReceived != r.Amount || (st.Currency != "" && !strings.EqualFold(st.Currency, r.Currency)) {
				logf("stripe_tap: succeeded payment does not match the sale")
				return pendingOut()
			}
			rec.State, rec.PI = "approved", st.ID
			_ = putTapAttempt(kv, rec)
			return tapOutcome{kind: tapApproved, ref: st.ID}
		case "requires_payment_method":
			craw, crc := b.providerCall("stripe", "session_cancel", []byte("{}"))
			var cs brokerStatus
			if crc < 0 || json.Unmarshal(craw, &cs) != nil || cs.Error.Code != "" || cs.Status != "canceled" {
				logf("stripe_tap: could not cancel the earlier attempt")
				return pendingOut()
			}
		case "canceled":
		default:
			return pendingOut() // processing, requires_capture, unknown, anything else
		}
	}

	if err := putTapAttempt(kv, tapAttempt{ID: r.Key, State: "pending", Amount: r.Amount, Currency: r.Currency, At: now}); err != nil {
		logf("stripe_tap: could not save the attempt record (%v), nothing tapped", err)
		return startFailedOut()
	}
	mark := func(state, pi string) {
		_ = putTapAttempt(kv, tapAttempt{ID: r.Key, State: state, Amount: r.Amount, Currency: r.Currency, PI: pi, At: now})
	}

	raw, rc := b.tapRequest()
	if rc == -2 {
		mark("declined", "") // not permitted / not bound: nothing was created
		logf("stripe_tap: tap_to_pay_request not permitted")
		return startFailedOut()
	}
	if rc < 0 {
		logf("stripe_tap: tap_to_pay_request host code %d", rc)
		return pendingOut()
	}
	var res tapResult
	if json.Unmarshal(raw, &res) != nil {
		return pendingOut()
	}
	if res.Error.Code != "" {
		if startFailedCodes[res.Error.Code] {
			mark("declined", "")
			logf("stripe_tap: not started (%s)", res.Error.Code)
			return startFailedOut()
		}
		return pendingOut()
	}
	switch res.Status {
	case "approved":
		if res.ProviderTxnID == "" {
			return pendingOut()
		}
		mark("approved", res.ProviderTxnID)
		logf("stripe_tap: tap approved %s", maskRef(res.ProviderTxnID))
		return tapOutcome{kind: tapApproved, ref: res.ProviderTxnID}
	case "declined":
		if res.DeclineCode == "amount_mismatch" {
			return pendingOut()
		}
		code := res.DeclineCode
		if code == "" {
			code = "declined"
		}
		mark("declined", "")
		logf("stripe_tap: tap declined (%s)", scrubRefs(code))
		return tapOutcome{kind: tapDeclined, code: code}
	case "cancelled", "canceled":
		mark("declined", "")
		return tapOutcome{kind: tapDeclined, code: declineCancelled}
	}
	return pendingOut() // pending, timeout, unknown status
}

// tapRefund refunds through the broker (op refund; the plugin only gives
// amount_minor, core binds the PaymentIntent and refund key). With
// refund_amount on the legs each leg goes exactly as core decided; otherwise
// the whole event amount. Fail closed on every doubt.
func tapRefund(b brokerAPI, p eventPayload) (refundID, decline string) {
	var amounts []int64
	if perLegMode(p) {
		legs, d := planLegs(p)
		if d != "" {
			return "", d
		}
		for _, l := range legs {
			amounts = append(amounts, l.Amount)
		}
	} else {
		if p.Amount <= 0 {
			return "", declineInvalidRefundAmount
		}
		amounts = []int64{p.Amount}
	}
	var first string
	for i, amt := range amounts {
		body, _ := json.Marshal(map[string]int64{"amount_minor": amt})
		raw, rc := b.providerCall("stripe", "refund", body)
		reason, id := "", ""
		switch {
		case rc < 0:
			reason = declineRefundDeny
		default:
			var rf struct {
				RefundID string `json:"refund_id"`
			}
			_ = json.Unmarshal(raw, &rf)
			id = rf.RefundID
			if code := brokerErrorCode(raw); code != "" {
				reason = code
			} else if id == "" {
				reason = declineRefundFail
			}
		}
		if reason != "" {
			if i > 0 {
				logf("stripe_tap: REFUND PARTIAL: %d of %d legs done (%s)", i, len(amounts), scrubRefs(reason))
				return "", declineRefundPartial
			}
			logf("stripe_tap: refund refused (%s)", scrubRefs(reason))
			return "", reason
		}
		if i == 0 {
			first = id
		}
		logf("stripe_tap: REFUNDED %d (%s)", amt, maskRef(id))
	}
	return first, ""
}
