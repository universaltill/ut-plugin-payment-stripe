package main

import (
	"strings"
	"testing"
)

// ut-docs#6438 D1: provider references never reach the log in full.
func TestMaskRefHidesFullReference(t *testing.T) {
	const ref = "pi_3Abcdefg1234567890"
	got := maskRef(ref)
	if got != "...7890" || strings.Contains(got, "pi_3") {
		t.Fatalf("maskRef(%q) = %q, want ...7890", ref, got)
	}
	if maskRef("abc") != "***" || maskRef("") != "***" {
		t.Fatalf("short refs must be fully hidden")
	}
}

func TestScrubRefsMasksIDsInProviderText(t *testing.T) {
	in := "No such payment_intent: pi_3Abcdefg1234567890 (txn 2f1c9a7e-1111-4222-8333-944455556666)"
	want := "No such payment_intent: ...7890 (txn ...6666)"
	if got := scrubRefs(in); got != want {
		t.Fatalf("scrubRefs = %q, want %q", got, want)
	}
	if got := scrubRefs("card_declined"); got != "card_declined" {
		t.Fatalf("plain decline code changed: %q", got)
	}
}
