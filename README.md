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
cancels the reader prompt and the PaymentIntent. A cancelled PaymentIntent can't
be charged, so a tap after that is refused. If the card was already being read,
the plugin waits for the result and reports a payment that went through as paid.
If Stripe gives no answer in time (a very slow network), the payment is declined
as `outcome_unknown`. Tap **Pay** again on the **same basket** after any card
timeout: the plugin checks the earlier payment first and approves it if it went
through, without charging again. If you change the basket instead, check the
reader or the Stripe dashboard before charging again (ut-docs#6304).

A card the reader declines (v1.5.0, ut-docs#6440) is declined at once with
Stripe's reason (for example `insufficient_funds`) instead of after the 18
seconds as `reader_timeout`.

## Refunds (v1.4.0, ut-docs#6438)

Refunding a Stripe sale on the till refunds the original PaymentIntent. The
plugin finds it in this order: the reference the till itself stored with the
sale (`transaction_ref`, `original_payments`, newer till versions), then the
plugin's own link file, then the per-sale keys written by v1.3.0 and older
(still read, never written again). Until the till stores the reference itself,
the plugin links each sale to its charge in 256 fixed files in its own blob
store (`blob:own`, at most 8 MiB, oldest first out). A sale that could not be
linked safely (for example two card sales of the same amount settling at the
same moment) is **not** linked; refunding it declines with
`unknown_original_payment` and must be done in the Stripe dashboard. The plugin
never guesses a charge. Before v1.4.0 a till stopped linking new sales after
about 1,000 card sales (the host allows a plugin 1,024 storage keys); update the
plugin by hand on each till, plugins do not auto-update.

## Build

```sh
bash scripts/build.sh   # -> bin/plugin.wasm (GOOS=wasip1 GOARCH=wasm)
```
