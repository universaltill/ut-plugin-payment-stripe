package main

// Payout reconciliation parsing (ut-docs#6579, ADR-0201 section 3). No
// //go:build tag: pure functions over already-fetched Stripe JSON, covered by
// a plain `go test`. They take no key and make no HTTP call: the Stripe key
// never leaves the cloud, so fetching (and the host event route) is #6578.
// Amounts are Stripe integer minor units, parsed as integers (a decimal or a
// quoted number is an error, never rounded). Errors never echo the body: a
// Stripe error message can carry the key.

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

var intRe = regexp.MustCompile(`^-?[0-9]{1,18}$`)

type minor int64

func (m *minor) UnmarshalJSON(b []byte) error {
	if !intRe.Match(b) {
		return errors.New("amount is not an integer in minor units")
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*m = minor(n)
	return nil
}

type stripePayout struct {
	ID          string
	Amount      int64
	Currency    string
	Status      string
	Automatic   bool
	ArrivalDate int64
}

// settledLine is the normalised line of ut-docs ref 5.3 (signed minor units).
type settledLine struct {
	TransactionRef string `json:"transaction_ref"`
	RefKind        string `json:"ref_kind"` // "payment_intent" or "charge" (unexpanded source)
	Kind           string `json:"kind"`     // sale, refund, chargeback, adjustment
	Gross          int64  `json:"gross"`
	Fee            int64  `json:"fee"`
	Net            int64  `json:"net"`
	Tip            int64  `json:"tip"`
	Currency       string `json:"currency"`
	OccurredAt     string `json:"occurred_at"` // RFC 3339 UTC, "" when unknown
	SettledAt      string `json:"settled_at"`
	PayoutRef      string `json:"payout_ref"`
}

type payoutSummary struct {
	PayoutRef   string `json:"payout_ref"`
	PayoutDate  string `json:"date"`   // YYYY-MM-DD (UTC)
	PayoutState string `json:"status"` // paid | pending
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
	LinesNet    int64  `json:"lines_net"`
	Balanced    bool   `json:"balanced"`
}

type cannotMatch struct {
	PayoutRef string `json:"payout_ref"`
	Amount    int64  `json:"amount"`
	Currency  string `json:"currency"`
	Reason    string `json:"reason"`
}

type reconcileResult struct {
	Lines       []settledLine   `json:"lines"`
	Payouts     []payoutSummary `json:"payouts"`
	CannotMatch []cannotMatch   `json:"cannot_match"`
}

var errBadReconcileBody = errors.New("reconcile: malformed Stripe response")

// parsePayouts reads one page of GET /v1/payouts. last is the id to pass as
// starting_after when more is true.
func parsePayouts(body []byte) (ps []stripePayout, more bool, last string, err error) {
	var in struct {
		Object  string          `json:"object"`
		Error   json.RawMessage `json:"error"`
		HasMore bool            `json:"has_more"`
		Data    []struct {
			ID          string `json:"id"`
			Amount      minor  `json:"amount"`
			Currency    string `json:"currency"`
			Status      string `json:"status"`
			Automatic   bool   `json:"automatic"`
			ArrivalDate int64  `json:"arrival_date"`
		} `json:"data"`
	}
	// Fail closed: a Stripe error body ({"error":...}) or anything that is not
	// a list must never read as "no payouts".
	if json.Unmarshal(body, &in) != nil || in.Object != "list" || len(in.Error) > 0 {
		return nil, false, "", errBadReconcileBody
	}
	for _, d := range in.Data {
		if !strings.HasPrefix(d.ID, "po_") {
			return nil, false, "", errBadReconcileBody
		}
		ps = append(ps, stripePayout{ID: d.ID, Amount: int64(d.Amount), Currency: strings.ToUpper(d.Currency),
			Status: d.Status, Automatic: d.Automatic, ArrivalDate: d.ArrivalDate})
		last = d.ID
	}
	return ps, in.HasMore, last, nil
}

// rfc3339 renders Stripe epoch seconds as the contract's RFC 3339 UTC string.
func rfc3339(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05Z")
}

func dateUTC(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format("2006-01-02")
}

func lineKind(t string) string {
	switch t {
	case "charge", "payment":
		return "sale"
	case "refund", "payment_refund":
		return "refund"
	case "dispute", "adjustment_dispute", "payment_failure_refund":
		return "chargeback"
	}
	return "adjustment"
}

// parseBalanceTxns reads one page of GET /v1/balance_transactions?payout=po_.
// The join id is the PaymentIntent when the source is expanded
// (expand[]=data.source) and otherwise the charge id, flagged RefKind
// "charge": never guessed. The payout's own row is skipped.
func parseBalanceTxns(body []byte, payoutRef string) (ls []settledLine, more bool, last string, err error) {
	var in struct {
		Object  string          `json:"object"`
		Error   json.RawMessage `json:"error"`
		HasMore bool            `json:"has_more"`
		Data    []struct {
			ID          string          `json:"id"`
			Amount      minor           `json:"amount"`
			Fee         minor           `json:"fee"`
			Net         minor           `json:"net"`
			Currency    string          `json:"currency"`
			Type        string          `json:"type"`
			Created     int64           `json:"created"`
			AvailableOn int64           `json:"available_on"`
			Source      json.RawMessage `json:"source"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &in) != nil || in.Object != "list" || len(in.Error) > 0 {
		return nil, false, "", errBadReconcileBody
	}
	for _, d := range in.Data {
		if !strings.HasPrefix(d.ID, "txn_") {
			return nil, false, "", errBadReconcileBody
		}
		last = d.ID
		if d.Type == "payout" {
			continue
		}
		if d.Net != d.Amount-d.Fee {
			return nil, false, "", errBadReconcileBody // amounts that do not add up are never guessed
		}
		ref, kind := sourceRef(d.Source)
		if ref == "" && lineKind(d.Type) != "adjustment" {
			return nil, false, "", errBadReconcileBody // a sale/refund/dispute with no id cannot be matched
		}
		ls = append(ls, settledLine{TransactionRef: ref, RefKind: kind, Kind: lineKind(d.Type),
			Gross: int64(d.Amount), Fee: int64(d.Fee), Net: int64(d.Net), Currency: strings.ToUpper(d.Currency),
			OccurredAt: rfc3339(d.Created), SettledAt: rfc3339(d.AvailableOn), PayoutRef: payoutRef})
	}
	return ls, in.HasMore, last, nil
}

func sourceRef(raw json.RawMessage) (ref, kind string) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, "charge"
	}
	var o struct {
		ID string `json:"id"`
		PI string `json:"payment_intent"`
	}
	if json.Unmarshal(raw, &o) != nil {
		return "", ""
	}
	if o.PI != "" {
		return o.PI, "payment_intent"
	}
	return o.ID, "charge"
}

// buildReconcile assembles the answer. Only automatic payouts with fetched
// lines are matched; instant/manual ones and automatic ones with no lines go
// to cannot_match (never dropped). A payout whose lines do not sum to its
// amount is returned with Balanced=false for the caller to flag.
func buildReconcile(payouts []stripePayout, byPayout map[string][]settledLine) reconcileResult {
	res := reconcileResult{Lines: []settledLine{}, Payouts: []payoutSummary{}, CannotMatch: []cannotMatch{}}
	for _, p := range payouts {
		if !p.Automatic {
			res.CannotMatch = append(res.CannotMatch, cannotMatch{p.ID, p.Amount, p.Currency, "payout_not_automatic"})
			continue
		}
		// Fail closed on the status: only paid / pending (in_transit) are
		// settlement; failed/canceled are cannot_match, anything else is unknown.
		state := ""
		switch p.Status {
		case "paid":
			state = "paid"
		case "pending", "in_transit":
			state = "pending"
		case "failed", "canceled":
			res.CannotMatch = append(res.CannotMatch, cannotMatch{p.ID, p.Amount, p.Currency, "payout_failed"})
			continue
		default:
			res.CannotMatch = append(res.CannotMatch, cannotMatch{p.ID, p.Amount, p.Currency, "unknown_payout_status"})
			continue
		}
		lines := byPayout[p.ID]
		if len(lines) == 0 {
			res.CannotMatch = append(res.CannotMatch, cannotMatch{p.ID, p.Amount, p.Currency, "no_balance_transactions"})
			continue
		}
		var net int64
		sameCur := true
		for _, l := range lines {
			net += l.Net
			sameCur = sameCur && l.Currency == p.Currency // mixed currency never balances
		}
		res.Lines = append(res.Lines, lines...)
		res.Payouts = append(res.Payouts, payoutSummary{PayoutRef: p.ID, PayoutDate: dateUTC(p.ArrivalDate), PayoutState: state,
			Amount: p.Amount, Currency: p.Currency, LinesNet: net, Balanced: sameCur && net == p.Amount})
	}
	return res
}
