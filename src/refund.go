package main

// Provider refund against the original PaymentIntent (ut-docs#6438). Untagged
// so the money path is covered by `go test`; main.go only wires the host.

import (
	"encoding/json"
	"fmt"
	"net/url"
)

const declineUnknownOriginal = "unknown_original_payment"

// stripeTooLarge are Stripe's definite "this charge cannot take that refund"
// answers; only these let a split-tender refund move on to the next leg.
func stripeTooLarge(code string) bool {
	return code == "charge_already_refunded" || code == "amount_too_large"
}

// refundOne asks Stripe to refund amount on one PaymentIntent. done=true on
// success (refID set). tooLarge=true when Stripe definitively says this
// charge cannot take it. Otherwise reason is the decline code.
func refundOne(api stripeAPI, pi string, amount int64, eventID string) (refID string, done, tooLarge bool, reason string) {
	// The till re-sends the same event id when the cashier retries a refund
	// whose reply was lost; Stripe then replays the first answer (the
	// refund) instead of refunding again (ut-docs#6598).
	idem := ""
	if eventID != "" {
		idem = "ut-refund-" + eventID + "-" + pi
	}
	body, status, ok := api.call("POST", "/v1/refunds", fmt.Sprintf("payment_intent=%s&amount=%d", url.QueryEscape(pi), amount), idem)
	if !ok {
		return "", false, false, "network_error"
	}
	if status == 401 {
		return "", false, false, declineInvalidKey
	}
	var rf struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &rf)
	switch rf.Status {
	case "succeeded", "pending":
		return rf.ID, true, false, ""
	}
	return "", false, stripeTooLarge(rf.Error.Code), firstNonEmpty(rf.Error.Code, "refund_failed")
}

// refundSale finds the original payment (refundRefs) and refunds it: one
// charge only, never split across two. With several candidate legs it moves
// on only after a definite "too large" answer; any other failure declines.
// It never guesses a charge: no reference means unknown_original_payment.
func refundSale(api stripeAPI, kv kvStore, links linkStore, p eventPayload) (refID, decline string) {
	refs := refundRefs(kv, links, p, "sale_pi:")
	if len(refs) == 0 {
		return "", declineUnknownOriginal
	}
	last := "refund_failed"
	for _, pi := range refs {
		id, done, tooLarge, reason := refundOne(api, pi, p.Amount, p.EventID)
		if done {
			logf("stripe: REFUNDED %d on %s (%s)", p.Amount, maskRef(pi), maskRef(id))
			return id, ""
		}
		last = reason
		if !tooLarge {
			return "", reason
		}
	}
	return "", last
}
