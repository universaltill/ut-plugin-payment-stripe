package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSaveAuthorizeResultDoesNotAddRecentToTheAnswer(t *testing.T) {
	kv := memKV{}
	fields := map[string]any{"provider": "stripe", "outcome": "approved", "transaction_ref": "pi_secret123"}
	saveAuthorizeResult(kv, fields, &recentEntry{ID: "e1", Ref: "pi_secret123", Amount: 100})
	if _, ok := fields["recent"]; ok {
		t.Fatal("answer map was given a recent key")
	}
	b, _ := json.Marshal(fields)
	if strings.Contains(string(b), "recent") {
		t.Fatalf("stdout answer carries recent: %s", b)
	}
	if got := loadRecent(kv); len(got) != 1 || got[0].Ref != "pi_secret123" {
		t.Fatalf("recent not kept in state: %+v", got)
	}
}

func TestDeclineAnswerCarriesDeclineCode(t *testing.T) {
	var a map[string]any
	if err := json.Unmarshal(declineAnswer(1250, "gbp", "outcome_unknown"), &a); err != nil {
		t.Fatal(err)
	}
	if a["decline_code"] != "outcome_unknown" || a["outcome"] != "declined" {
		t.Fatalf("answer: %v", a)
	}
}
