package main

import "encoding/json"

// onlineDecision turns the no-reader charge's Stripe answer into a verdict:
// the PaymentIntent id when approved (reason ""), else the decline reason.
// Untagged so `go test` covers it; main.go only wires the host (ut-docs#6570).
// Stripe's error message is never read: it echoes a masked key.
func onlineDecision(status int, ok bool, body []byte) (id, reason string) {
	if !ok {
		return "", "network_error"
	}
	if status == 401 {
		return "", declineInvalidKey
	}
	var pi struct {
		Status string `json:"status"`
		ID     string `json:"id"`
		Error  struct {
			Code        string `json:"code"`
			DeclineCode string `json:"decline_code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &pi)
	if pi.Status == "succeeded" {
		return pi.ID, ""
	}
	return "", firstNonEmpty(pi.Error.DeclineCode, pi.Error.Code, "declined")
}
