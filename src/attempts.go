package main

// Attempt records for retry settlement (ut-docs#6304). They live in ONE
// bounded storage key: the host caps a plugin at 1024 keys and gives the
// guest no delete (universal-till internal/data/plugin_repo.go
// StorageMaxKeys), so a key per attempt would fill the quota. Only the
// last maxAttempts tenders are kept — a retry comes seconds or minutes
// after its timeout, never hundreds of sales later.

import "encoding/json"

const (
	attemptsKey = "attempts"
	maxAttempts = 8
)

type attemptEntry struct {
	ID  string `json:"id"`            // the till's authorize request id
	Ref string `json:"ref"`           // provider reference (PaymentIntent / client_transaction_id)
	Seq int    `json:"seq,omitempty"` // provider-side attempt counter for this id
	At  int64  `json:"at,omitempty"`  // unix seconds the provider request started
}

func loadAttempts(kv kvStore) []attemptEntry {
	var list []attemptEntry
	_ = json.Unmarshal([]byte(kv.get(attemptsKey)), &list)
	return list
}

func findAttempt(kv kvStore, id string) (attemptEntry, bool) {
	if id == "" {
		return attemptEntry{}, false
	}
	for _, e := range loadAttempts(kv) {
		if e.ID == id {
			return e, true
		}
	}
	return attemptEntry{}, false
}

// putAttempt stores e (replacing any entry with its id), newest last,
// trimmed to maxAttempts. A no-op for an empty id (an older till sends none).
func putAttempt(kv kvStore, e attemptEntry) {
	if e.ID == "" {
		return
	}
	list := make([]attemptEntry, 0, maxAttempts)
	for _, old := range loadAttempts(kv) {
		if old.ID != e.ID {
			list = append(list, old)
		}
	}
	list = append(list, e)
	if len(list) > maxAttempts {
		list = list[len(list)-maxAttempts:]
	}
	b, _ := json.Marshal(list)
	if err := kv.put(attemptsKey, b); err != nil {
		logf("stripe: could not save the attempt record (%v); a retry of this sale may start a new charge", err)
	}
}
