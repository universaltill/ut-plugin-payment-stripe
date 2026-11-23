# Stripe Card Payments — Universal Till plugin

Take card payments through **Stripe**, on the authorize-before-tender seam: the
till asks Stripe to charge the amount **before** completing the sale, and only
finishes if Stripe approves (a decline leaves the basket intact).

## Configure (plugin settings)

- **Secret key** (`stripe_secret_key`) — your Stripe secret key (`sk_test_…` for testing,
  `sk_live_…` in production). One plugin, per-merchant key — nothing hardcoded.
- **Currency** (`currency`) — `auto` (default: the sale's currency, else the shop's) or a pinned code; a mismatch with the sale declines `currency_mismatch`.
- **Card reader ID** (`stripe_reader_id`) — the Stripe Terminal reader this till drives (server-driven
  readers: BBPOS WisePOS E, Stripe Reader S700). Set per till. Without it the
  plugin makes an online charge instead (test keys only make sense there).

Each field shows its name and a help line on the settings page (the setting key
shows on hover). A wrong, revoked or expired secret key declines every payment
with the reason `invalid_secret_key` (HTTP 401 from Stripe); the reason appears in
the till log, not on the cashier's screen. The labels are English only for now
(`locales/en.json`, shipped in the package); other languages come later through
Weblate (ADR-0160 §2).

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

### Split-tender refunds, one amount per leg (v1.8.0, ut-docs#6603)

When the till names an amount for each original payment (`refund_amount` on
`original_payments`, sent only when a sale has two or more Stripe payments), the
plugin refunds exactly that amount on each PaymentIntent, in order, and never
chooses a payment itself. The total must equal the refund and no leg may exceed
what it took (amount plus tip). Otherwise it declines before calling Stripe:
`invalid_refund_amount`, `unknown_original_payment` (a leg without a
reference) or `too_many_original_payments` (more than 16 legs). If a later leg
fails after an earlier one was refunded, it declines `refund_partial`: tap
Refund again on the same screen to finish, and do not refund the rest another
way. A retry only helps when the failed leg hit a network or temporary error:
Stripe replays a definite refusal (for example `amount_too_large`) under the
same key, so then finish that leg in the Stripe Dashboard. The till shows its generic "declined" text until it reads this code for
refunds. Each leg uses `Idempotency-Key: ut-refund-<event>-<PaymentIntent>-<amount>`,
so a retry replays the legs already refunded instead of refunding them again.
**Limit:** Stripe keeps idempotency keys for at least 24 hours; retrying the
same refund screen after that can refund a partly done leg a second time, so
finish a `refund_partial` the same day. Reversals ignore `refund_amount`. Per-leg
refunds are tested against fakes of Stripe's documented behaviour, not a live
account.

## Reader per call and reader state (v1.9.0, ut-docs#6566)

A till with a reader registry names the Terminal reader for each card sale
(`reader_id` in the authorize payload). The plugin uses it instead of
`stripe_reader_id`; with no `reader_id` (an older till, or a single-reader
till) it uses the setting exactly as before, so v1.x stays backward compatible.
The id must look like `tmr_` plus 1 to 64 letters or digits. A payload id that
does not is declined with `invalid_reader` before any Stripe call, and the
setting is **not** used instead (that would charge a different physical
reader). The id goes into a Stripe URL path, which is why it is checked.

The plugin also answers the read-only hook `payment.stripe.readers`
(`{"op":"state","readers":["tmr_..."]}`, at most 16 per call) with
`ready`, `busy` (a reader action is in progress), `offline`, or `unknown`
(any unclear answer, or a rejected key). A reader Stripe does not know is
`offline` with `reader_unreachable`. Nothing is changed on the reader.

Refunds still go by the original charge (`transaction_ref`). A `reader_id` on
`original_payments[]` is accepted and ignored: a removed or offline reader
never blocks a refund. Amounts stay integer minor units; reader ids are logged
masked (last 4) at most.

## Build

```sh
bash scripts/build.sh   # -> bin/plugin.wasm (GOOS=wasip1 GOARCH=wasm)
```

## Tap to Pay on iPhone (stripe_tap, v1.10.0)

A second payment entry, "Tap to Pay on iPhone", for a till whose phone is the card reader. It needs the permissions `device:tap_to_pay` and `net:payment_broker`, which a manager grants (they are not granted automatically). It holds no Stripe key and never calls Stripe itself: it asks the till's payment broker, which confirms every payment before a sale completes. The `net:api.stripe.com` permission stays for the original "Card (Stripe)" entry only. Needs till 0.36.3 or newer.
