# Code review: ut-plugin-payment-stripe 1.10.0 stripe_tap (Tap to Pay finish, ut-docs#6454/#6456/#6458)

Reviewer: combined Reviewer (Sonnet 5.5, not the Dev, not the Tester), 2026-11-23. Diff read in full against
ADR-0198, ADR-0203 and `reference/contracts/stripe-tap-to-pay-v1.md`; targeted tests re-run. Pre-flight: plan/price
n/a; platform claim n/a; no secret, real shop name or key literal in the diff (test keys are fake placeholders).

## What shipped
Second payment entry `stripe_tap` (`config {tap:true, provider:"stripe"}`), permissions `device:tap_to_pay` and
`net:payment_broker`, `src/tap_flow.go` (pure, tested with a fake broker) and `src/tap.go` (host seam). Holds no
Stripe key, no HTTP call; every outcome is confirmed by core. Version 1.10.0, `min_pos_version` 0.36.3.

## Findings
- No blocking finding. Verified: attempt record written before the tap; retry asks `session_status` first;
  succeeded approves only if id and amount match the sale; unknown outcomes are pending (`outcome_unknown`), nothing
  charged is `start_failed`; the tap host call is never repeated; refund fails closed and logs partials.
- Deferred (core, not this plugin): refund over 2+ tap legs of the same method is unbound in core and refused by the
  broker (fails closed, no money moves; operator refunds in the Stripe dashboard). Needs a core card.

## Verification
go vet, gofmt, go test, validate.sh, wasm build, i18n guard: ok. Re-run `check-version-bump.sh` after the commit.

## Verdict
GO (release tag v1.10.0 after merge; core bundling is a separate card).
