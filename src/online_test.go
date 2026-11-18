package main

import "testing"

func TestOnlineDecision(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		ok     bool
		id     string
		reason string
	}{
		{"wrong key", 401, invalidKeyBody, true, "", "invalid_secret_key"},
		{"card error", 402, `{"error":{"code":"card_declined","decline_code":"insufficient_funds"}}`, true, "", "insufficient_funds"},
		{"succeeded", 200, `{"id":"pi_1","status":"succeeded"}`, true, "pi_1", ""},
		{"no code", 400, `{"error":{"type":"invalid_request_error"}}`, true, "", "declined"},
		{"transport", 0, ``, false, "", "network_error"},
	}
	for _, c := range cases {
		id, reason := onlineDecision(c.status, c.ok, []byte(c.body))
		if id != c.id || reason != c.reason {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, id, reason, c.id, c.reason)
		}
	}
}
