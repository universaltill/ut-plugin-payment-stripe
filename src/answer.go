package main

import "encoding/json"

// declineAnswer is the JSON printed on stdout for a decline (exit 2). Core
// reads it on a non-zero exit too, so decline_code (e.g. outcome_unknown)
// reaches the till (ut-docs#6600).
func declineAnswer(amount int64, currency, reason string) []byte {
	b, _ := json.Marshal(declineFields(amount, currency, reason))
	return b
}

func declineFields(amount int64, currency, reason string) map[string]any {
	return map[string]any{
		"provider": "stripe", "amount": amount, "currency": currency,
		"outcome": "declined", "decline_code": reason,
	}
}
