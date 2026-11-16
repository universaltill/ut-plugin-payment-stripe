# ut-plugin-payment-stripe — notes

A WASM (`GOOS=wasip1 GOARCH=wasm`) **payment** plugin. Hooks
`payment.stripe.authorize` (blocking, pre-tender): exit 0 = approved, non-zero
= declined. Reads `stripe_secret_key` + `currency` via the `settings_get` host
function; POSTs a confirmed PaymentIntent to `https://api.stripe.com/v1/payment_intents`
via `http_request` (needs `net:api.stripe.com`); form-encoded body; parses
`status == "succeeded"`. Template for other API-based gateways (iyzico, etc.):
swap the endpoint, auth, and request/response shaping. Build: `scripts/build.sh`.

**Reader timeout (ut-docs#6304, v1.3.0):** the Terminal flow lives in
`src/terminal.go` (untagged, `go test`-covered with a fake Stripe/clock in
`terminal_test.go`; CI runs `go test ./...`). The till kills an authorize call
at 30 s (`paymentGateTimeout`), so the flow polls 18 s, then cancels the reader
action and the PaymentIntent and re-checks until 24 s (no call starts after that): paid -> approve, cancelled
-> decline `reader_timeout`, still processing -> decline `outcome_unknown`. The
PaymentIntent is kept under `attempt:<event id>`; the till re-sends the same id
on a re-tap of Pay for the same basket (ut-docs#1762), and the retry settles that
PaymentIntent before creating another (Idempotency-Key `ut-<id>-<seq>`). Never
move the poll past the host gate or drop a payment that succeeded late.
