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

**Wire-level contract suite (ut-docs#6565, v1.5.1):** `src/contract_test.go` runs the Terminal flow and refund over real HTTP, form-encoded (paths, bodies, idempotency; not headers/auth), against a fake server that follows Stripe's DOCUMENTED behaviour (create body, Idempotency-Key replay incl. stored errors, test-helper only for `sk_test_`, reader decline, cancel of reader action + PaymentIntent, refund). It is NOT a recording of Stripe: the simulated-reader run against real Stripe test mode is the live path below (ut-docs#7081).

**Settings labels + invalid key (ut-docs#6570, v1.6.0):** setting `label`/`help` are `payment_stripe.<name>.label`/`.help` keys in `locales/en.json` (English only, ADR-0160 §2), pinned by `src/settings_labels_test.go`; `scripts/package.sh` ships `locales/` and `check-version-bump.sh` counts `locales/*`. `stripeAPI.call` returns `(body, status, ok)`; HTTP 401 declines with `invalid_secret_key` (online charge via `onlineDecision` in `src/online.go`, reader create, refund). Never log Stripe's error message: it echoes the key. A 401 during settle/poll stays `outcome_unknown`.

**Fable fixes (ut-docs#6598/#6604/#6606/#6600, v1.7.0):** refund sends `Idempotency-Key: ut-refund-<event id>-<PaymentIntent>` so a lost-reply retry replays Stripe's first answer (no double refund, a full refund stays recordable, no move to another leg). `currency` is a choice with `auto` (`src/currency.go`, from the SumUp plugin; needs `view:shop`); mismatch declines `currency_mismatch`. `saveAuthorizeResult` copies the answer map so stdout never carries `recent`. Declines print `decline_code` (e.g. `outcome_unknown`) on stdout at exit 2 (`src/answer.go`); core reads stdout on non-zero exit.

**Per-leg refunds (ut-docs#6603, v1.8.0):** when any `original_payments[i].refund_amount` is present (`*int64`, presence-aware) on a non-reversal refund, `src/refundlegs.go` refunds exactly that amount per leg, in order, with no "too large -> next leg" fallback (ADR-0196 d.4: core decides, the plugin never picks). `planLegs` validates everything before any call (`too_many_original_payments` > 16, `unknown_original_payment`, `invalid_refund_amount`). Per-leg key `ut-refund-<evt>-<pi>-<refund_amount>` (the amount avoids clashing with a stored v1.7.0 `amount_too_large`); the legacy key and path are unchanged when the field is absent. A failure after a done leg declines `refund_partial`; the stateless retry replays done legs (Stripe keeps keys >= 24 h only). Never log a Stripe message or a raw ref (`scrubRefs`/`maskRef`).

**Reader per call + state (ut-docs#6566, v1.9.0):** `src/readers.go` (untagged, `readers_test.go`). `chooseReader(payload reader_id, stripe_reader_id)`: payload wins, missing/blank falls back to the setting, a present id not matching `^tmr_[A-Za-z0-9]{1,64}$` declines `invalid_reader` with no API call and NO fallback (the id goes into URL paths; strict, no trimming of a non-empty id). Hook `payment.stripe.readers` (manifest declaration = the capability core checks) answers `{"readers":[{reader_id,state,reason?}]}`: GET `/v1/terminal/readers/{id}`, online+no action ready, action busy, offline/404 offline (404 reason `reader_unreachable`), anything else unknown; max 16 refs; malformed refs never reach a URL. Refund ignores `reader_id` (also on `original_payments[]`): it goes by `transaction_ref` and must never block.

**Tap to Pay on iPhone entry (ut-docs#6456, v1.10.0):** second payment entry `stripe_tap` (`config {tap:true, provider:"stripe"}`, hooks `payment.stripe_tap.{authorize,requested,refund,readers}`), permissions `device:tap_to_pay` + `net:payment_broker` (core does NOT auto-grant them; a manager does). It holds no Stripe key and makes no HTTP call (`net:api.stripe.com` stays for the legacy `stripe` entry only). `main()` routes `isTapEvent` to `tapMain` BEFORE the `no_secret_key` decline. Pure logic in `src/tap_flow.go` (untagged, `tap_flow_test.go`, fake `brokerAPI`); host seam `src/tap.go` (wasip1): `payment_provider_call` / `tap_to_pay_request`, one 64 KiB buffer, never re-called (a repeat would start a second card sheet). Attempt records live in ONE bounded key `tap_attempts` (last 8) keyed by the gate's event id, written BEFORE the tap; a retry asks `session_status` first (succeeded -> approve with no tap, requires_payment_method -> `session_cancel` then tap, anything else -> pending). Pending = decline code `outcome_unknown` (core's pending screen); nothing-charged = `start_failed`. Refund goes via the broker `refund` op with `amount_minor` only (per leg when `refund_amount` is present), fail closed. `min_pos_version` 0.36.3 (first till with the host functions).

**Simulated reader live path (ut-docs#7081, v1.10.1):** `src/simreader_live_test.go` (build tag `stripelive`, env `STRIPE_TEST_TERMINAL_KEY`) registers a `simulated-s700` reader in a test location and drives the real `terminalCharger.run()`, `refundSale`/`refundLegs` and `readerStates` over HTTP with a real clock; it skips green without the key. Client and guards live untagged in `src/livestripe_test.go` (offline unit tests run in every `go test`): `sk_test_` ONLY (`rk_test_` refused, because the flow sends `present_payment_method` only for `sk_test_`), `https://api.stripe.com` only, `Stripe-Version: 2025-05-28.basil` pinned in the TEST client only (production sends none), failures printed via `errSummary` (never Stripe's message or the key). Decline and no-tap are a test-side `presentOverride` decorator, no production seam. Workflow `simulated-reader.yml` (dispatch + weekly, never a PR trigger, never required) runs in environment `stripe-test` (environment secret, branch main only; job `if` main) and skips with a notice until the owner adds the secret; the first real run is pending that. Details: `docs/simulated-reader.md`.

**Reconcile parse (ut-docs#6579, unreleased, no manifest bump):** `src/reconcile.go` (untagged, `reconcile_test.go`) holds pure, key-agnostic parsers over already-fetched Stripe JSON: `parsePayouts`, `parseBalanceTxns` (signed integer minor units; a decimal or quoted amount is an error), `buildReconcile` (automatic payouts matched; instant/manual or no-lines -> `cannot_match`; `Balanced` = lines net equals payout amount). Join id is the PaymentIntent when the balance-txn source is expanded, else the charge id flagged `ref_kind: charge`; never guessed. No host wiring (event route + fetch = #6578; the key never leaves the cloud). Identifier proof against test mode waits for the owner key (#7081).
