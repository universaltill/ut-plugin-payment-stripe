# Stripe Card Payments — Universal Till plugin

Take card payments through **Stripe**, on the authorize-before-tender seam: the
till asks Stripe to charge the amount **before** completing the sale, and only
finishes if Stripe approves (a decline leaves the basket intact).

## Configure (plugin settings)

- `stripe_secret_key` — your Stripe secret key (`sk_test_…` for testing,
  `sk_live_…` in production). One plugin, per-merchant key — nothing hardcoded.
- `currency` — ISO code (default `gbp`; e.g. `aed`, `try`, `usd`).

Grant it `net:api.stripe.com` (declared in the manifest).

## Test mode

Deterministic outcomes, like a demo terminal — no real money:
- amount minor units ending in **13** → Stripe's decline test card → **declined**
- anything else → Stripe's approve test card → **approved**

Verified end-to-end against the live Stripe test API (real PaymentIntents:
approve → `succeeded`, decline → `generic_decline`).

## Production / real cards

This reference uses Stripe **test payment methods** (card-not-present) to prove
the integration. A real deployment swaps the test `payment_method` for a live
card source — **Stripe Terminal** (a certified reader) is the POS path. The
authorize/approve/decline logic and settings are unchanged.

### Reader timeout (late taps)

The till waits 30 seconds for a card payment. With a reader set
(`stripe_reader_id`), the customer has about 18 seconds to tap; then the plugin
cancels the reader prompt and the PaymentIntent, so a tap after the till gave up
is refused, never charged. If the card was already being processed, the plugin
waits for the result and reports a payment that went through as paid. If Stripe
still has no answer, the payment is declined as `outcome_unknown`: tap **Pay**
again on the same basket and the plugin checks that payment first (it approves
it if it went through, and never charges twice). If you change the basket
instead, check the reader or the Stripe dashboard before charging again.

## Build

```sh
bash scripts/build.sh   # -> bin/plugin.wasm (GOOS=wasip1 GOARCH=wasm)
```
