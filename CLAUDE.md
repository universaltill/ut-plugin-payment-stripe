# ut-plugin-payment-stripe — notes

A WASM (`GOOS=wasip1 GOARCH=wasm`) **payment** plugin. Hooks
`payment.stripe.authorize` (blocking, pre-tender): exit 0 = approved, non-zero
= declined. Reads `stripe_secret_key` + `currency` via the `settings_get` host
function; POSTs a confirmed PaymentIntent to `https://api.stripe.com/v1/payment_intents`
via `http_request` (needs `net:api.stripe.com`); form-encoded body; parses
`status == "succeeded"`. Template for other API-based gateways (iyzico, etc.):
swap the endpoint, auth, and request/response shaping. Build: `scripts/build.sh`.
