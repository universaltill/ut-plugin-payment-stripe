package main

// Recent approvals and settle linking (ut-docs#6438).
//
// The old settle step trusted the single shared `last_txn` key: if sale B's
// authorize ran before sale A's settle event, A was linked to B's charge and a
// refund of A would refund B. Now `last_txn` also carries `recent`, up to 8
// approvals (id = authorize event id). Settle links a sale only when exactly
// one approval fits, or exactly by id when core sends authorize_request_id;
// otherwise it links nothing and logs. A missing link is a refund declined
// ("refund it in the provider dashboard"), never a refund on the wrong charge.

import (
	"encoding/json"
	"strings"
	"time"
)

const (
	lastTxnKey        = "last_txn"
	maxRecent         = 8
	settleMatchWindow = 600 // seconds an approval stays matchable by amount
)

type recentEntry struct {
	ID     string `json:"id"`
	Ref    string `json:"ref"`
	Amount int64  `json:"amount"`
	Tip    int64  `json:"tip,omitempty"`
	At     int64  `json:"at"`
	Used   bool   `json:"used,omitempty"`
}

func loadRecent(kv kvStore) []recentEntry {
	var cur struct {
		Recent []recentEntry `json:"recent"`
	}
	_ = json.Unmarshal([]byte(kv.get(lastTxnKey)), &cur) // a v1.3.0 value has no "recent": empty
	return cur.Recent
}

// updateLastTxn rewrites last_txn: top (the authorize answer; nil keeps the
// stored top-level fields, which a v1.3.0 plugin also reads) plus the edited
// recent list.
func updateLastTxn(kv kvStore, top map[string]any, edit func([]recentEntry) []recentEntry) error {
	raw := kv.get(lastTxnKey)
	var cur struct {
		Recent []recentEntry `json:"recent"`
	}
	_ = json.Unmarshal([]byte(raw), &cur)
	if top == nil {
		if json.Unmarshal([]byte(raw), &top) != nil || top == nil {
			top = map[string]any{}
		}
	}
	top["recent"] = edit(cur.Recent)
	b, _ := json.Marshal(top)
	return kv.put(lastTxnKey, b)
}

// addRecent appends e newest last, replacing an entry with the same id (a
// retry of one authorize never duplicates) and keeping the newest maxRecent.
func addRecent(list []recentEntry, e recentEntry) []recentEntry {
	out := make([]recentEntry, 0, len(list)+1)
	for _, o := range list {
		if o.ID != e.ID || e.ID == "" {
			out = append(out, o)
		}
	}
	out = append(out, e)
	if len(out) > maxRecent {
		out = out[len(out)-maxRecent:]
	}
	return out
}

// saveAuthorizeResult persists the authorize answer as last_txn (top-level
// fields as before) and, for an approval, appends it to recent. A failed write
// is logged: the sale itself is never affected.
func saveAuthorizeResult(kv kvStore, top map[string]any, approval *recentEntry) {
	err := updateLastTxn(kv, top, func(l []recentEntry) []recentEntry {
		if approval != nil {
			return addRecent(l, *approval)
		}
		return l
	})
	if err != nil {
		logf("stripe: could not save last_txn (%v); a refund of this sale may need the Stripe dashboard", err)
	}
}

// settleSale links a just-completed sale to its approval. Never blocks or
// fails the sale; core already holding the reference (transaction_ref) means
// nothing to store.
func settleSale(kv kvStore, links linkStore, p eventPayload, now time.Time) {
	if p.TransactionRef != "" || p.SaleID == "" {
		return
	}
	recent := loadRecent(kv)
	var matches []int
	for i, e := range recent {
		switch {
		case p.AuthorizeRequestID != "":
			if e.ID == p.AuthorizeRequestID {
				matches = append(matches, i)
			}
		case !e.Used && now.Unix()-e.At <= settleMatchWindow && now.Unix()-e.At >= -settleMatchWindow &&
			(e.Amount == p.Amount || e.Amount+e.Tip == p.Amount):
			matches = append(matches, i)
		}
	}
	if len(matches) != 1 {
		why := "no match"
		if len(matches) > 1 {
			why = "ambiguous"
		}
		logf("stripe: sale %s not linked (%s); refund it in the Stripe dashboard", maskRef(p.SaleID), why)
		return
	}
	e := recent[matches[0]]
	if err := links.add(p.SaleID, e.Ref, now); err != nil {
		logf("stripe: sale %s not linked (%v); refund it in the Stripe dashboard", maskRef(p.SaleID), err)
	}
	// Used either way: a failed link must never let a later sale claim it.
	if err := updateLastTxn(kv, nil, func(l []recentEntry) []recentEntry {
		for i := range l {
			if l[i].ID == e.ID {
				l[i].Used = true
			}
		}
		return l
	}); err != nil {
		logf("stripe: could not mark approval used (%v)", err)
	}
}

// refundRefs returns the provider references to try for a refund, best first.
// Order (ut-docs#6438): core's original_payments, then (reversal) the
// approval named by authorize_request_id, else the link bucket, else the
// legacy per-sale key written by v1.3.0 and older.
func refundRefs(kv kvStore, links linkStore, p eventPayload, legacyPrefix string) []string {
	if len(p.OriginalPayments) > 0 {
		if p.Reversal {
			if r := p.OriginalPayments[0].TransactionRef; r != "" {
				markUsed(kv, p.AuthorizeRequestID)
				return []string{r}
			}
		}
		var big, small []string
		for _, o := range p.OriginalPayments {
			if o.TransactionRef == "" {
				continue
			}
			if o.Amount+o.TipAmount >= p.Amount {
				big = append(big, o.TransactionRef)
			} else {
				small = append(small, o.TransactionRef)
			}
		}
		if refs := append(big, small...); len(refs) > 0 {
			return refs
		}
	}
	if p.Reversal {
		if p.AuthorizeRequestID == "" {
			return nil
		}
		for _, e := range loadRecent(kv) {
			if e.ID == p.AuthorizeRequestID && e.Ref != "" {
				markUsed(kv, e.ID)
				return []string{e.Ref}
			}
		}
		return nil
	}
	if p.OriginalSaleID == "" {
		return nil
	}
	if r, ok := links.find(p.OriginalSaleID); ok {
		return []string{r}
	}
	if r := strings.TrimSpace(kv.get(legacyPrefix + p.OriginalSaleID)); r != "" {
		return []string{r}
	}
	return nil
}

func markUsed(kv kvStore, id string) {
	if id == "" {
		return
	}
	_ = updateLastTxn(kv, nil, func(l []recentEntry) []recentEntry {
		for i := range l {
			if l[i].ID == id {
				l[i].Used = true
			}
		}
		return l
	})
}
