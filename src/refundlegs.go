package main

// Per-leg refunds (ut-docs#6603, ADR-0196 d.4): when core sends
// `refund_amount` on the legs of original_payments, it has decided what each
// leg gets, so the plugin refunds exactly that per leg and never picks one.
// Untagged: covered by `go test`.

import (
	"fmt"
	"math"
)

const (
	declineRefundPartial       = "refund_partial"
	declineInvalidRefundAmount = "invalid_refund_amount"
	declineTooManyLegs         = "too_many_original_payments"
	maxRefundLegs              = 16
)

// perLegMode: a sale refund (never a reversal) where core named an amount for
// at least one leg. Otherwise refundSale runs exactly as before.
func perLegMode(p eventPayload) bool {
	if p.Reversal {
		return false
	}
	for _, o := range p.OriginalPayments {
		if o.RefundAmount != nil {
			return true
		}
	}
	return false
}

type refundLeg struct {
	Ref    string
	Amount int64
	Full   bool // Amount == the leg's whole charge (amount + tip)
}

// planLegs validates the whole plan before any provider call.
func planLegs(p eventPayload) ([]refundLeg, string) {
	if len(p.OriginalPayments) > maxRefundLegs {
		return nil, declineTooManyLegs
	}
	for _, o := range p.OriginalPayments {
		if o.TransactionRef == "" {
			return nil, declineUnknownOriginal
		}
	}
	var legs []refundLeg
	var sum int64
	for _, o := range p.OriginalPayments {
		if o.RefundAmount == nil || *o.RefundAmount <= 0 || o.Amount < 0 || o.TipAmount < 0 {
			return nil, declineInvalidRefundAmount
		}
		if o.Amount > math.MaxInt64-o.TipAmount {
			return nil, declineInvalidRefundAmount
		}
		whole := o.Amount + o.TipAmount
		r := *o.RefundAmount
		if r > whole || sum > math.MaxInt64-r {
			return nil, declineInvalidRefundAmount
		}
		sum += r
		legs = append(legs, refundLeg{Ref: o.TransactionRef, Amount: r, Full: r == whole})
	}
	if len(legs) == 0 || sum != p.Amount {
		return nil, declineInvalidRefundAmount
	}
	return legs, ""
}

// legRefundKey is the per-leg Idempotency-Key. The amount is part of the key
// so a v1.7.0 key (same event and PaymentIntent, whole amount, stored by
// Stripe as amount_too_large for 24 h) can never clash with it.
func legRefundKey(eventID, pi string, amount int64) string {
	if eventID == "" {
		return ""
	}
	return fmt.Sprintf("ut-refund-%s-%s-%d", eventID, pi, amount)
}

// refundLegs refunds each leg's own amount, in order, stopping at the first
// failure. Stateless: a retry re-sends every leg with the same keys and Stripe
// replays the ones already done. A failure after a done leg is refund_partial.
func refundLegs(api stripeAPI, p eventPayload) (refID, decline string) {
	legs, d := planLegs(p)
	if d != "" {
		return "", d
	}
	var ids []string
	for i, l := range legs {
		id, done, _, reason := refundOne(api, l.Ref, l.Amount, legRefundKey(p.EventID, l.Ref, l.Amount))
		if !done {
			if i > 0 {
				logf("stripe: REFUND PARTIAL: %d of %d legs done, leg on %s failed (%s)", i, len(legs), maskRef(l.Ref), scrubRefs(reason))
				return "", declineRefundPartial
			}
			return "", reason
		}
		ids = append(ids, id)
		logf("stripe: REFUNDED %d on %s (%s)", l.Amount, maskRef(l.Ref), maskRef(id))
	}
	return ids[0], ""
}
