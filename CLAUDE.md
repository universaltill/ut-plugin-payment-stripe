# ut-plugin-payment-stripe — notes

A WASM (`GOOS=wasip1 GOARCH=wasm`) **payment** plugin. Hooks
`payment.stripe.authorize` (blocking, pre-tender): exit 0 = approved, non-zero
= declined. Reads `stripe_secret_key` + `currency` via the `settings_get` host
function; POSTs a confirmed PaymentIntent to `https://api.stripe.com/v1/payment_intents`
via `http_request` (needs `net:api.stripe.com`); form-encoded body; parses
`status == "succeeded"`. Template for other API-based gateways (iyzico, etc.):
swap the endpoint, auth, and request/response shaping. Build: `scripts/build.sh`.

**Early decline (ut-docs#6440, v1.5.0):** the poll reads `last_payment_error` on the PaymentIntent (`state()`); status `requires_payment_method` + an error + reader action `failed` (a soft decline keeps the action in progress, e.g. PIN prompt: keep polling) = card declined -> `settle` (cancel reader + PI; a tap that slipped in is still approved) then decline with `decline_code` (else `code`). Not verified against a live reader (`blocked:env` item on #6440).

**Reader timeout (ut-docs#6304, v1.3.0):** the Terminal flow lives in
`src/terminal.go` (untagged, `go test`-covered with a fake Stripe/clock in
`terminal_test.go`; CI runs `go test ./...`). The till kills an authorize call
at 30 s (`paymentGateTimeout`), so the flow polls 18 s, then cancels the reader
action and the PaymentIntent and re-checks until 24 s (no call starts after
that): paid -> approve, cancelled -> `reader_timeout`, still processing ->
`outcome_unknown`. PaymentIntents are kept in ONE bounded storage key
(`attempts`, last 8; host cap is 1024 keys, no delete) under the till's request
id, which the till re-sends on a re-tap of Pay for the same basket
(ut-docs#1762); the retry settles that PaymentIntent before creating another
(Idempotency-Key `ut-<id>-<seq>`, seq bumped after a cancel or a create error,
since Stripe replays a stored error for 24 h). A definite reader refusal cancels
only the PaymentIntent, never `cancel_action`. Never move the poll past the host
gate or drop a payment that succeeded late.

**Refund links (ut-docs#6438, v1.4.0):** never one storage key per sale or
event (host cap 1,024 keys, no guest delete). `src/links.go` keeps sale ->
PaymentIntent in 256 fixed `blob:own` buckets (32 KiB each, FIFO);
`src/recent.go` keeps the last 8 approvals in `last_txn` (`recent`) and links a
sale only on a unique match (amount/window) or exactly by `authorize_request_id`,
never by the bare last approval. Core's `transaction_ref` / `original_payments`
are used first and need no plugin storage. Refund order and the decline
`unknown_original_payment` live in `src/refund.go`. All of it is untagged and
tested against fakes that enforce the real host caps (`refundlink_test.go`).

**Wire-level contract suite (ut-docs#6565, v1.5.1):** `src/contract_test.go` runs the Terminal flow and refund over real HTTP, form-encoded (paths, bodies, idempotency; not headers/auth), against a fake server that follows Stripe's DOCUMENTED behaviour (create body, Idempotency-Key replay incl. stored errors, test-helper only for `sk_test_`, reader decline, cancel of reader action + PaymentIntent, refund). It is NOT a recording of Stripe: the simulated-reader run against real Stripe test mode waits for Key Vault `stripe-test-terminal-key` (not present on 2026-11-17).
