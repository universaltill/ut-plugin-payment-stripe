# Simulated reader (Stripe test mode)

ut-docs#7081. An opt-in test that runs the plugin's real Terminal flow against
Stripe's **simulated S700** reader (`simulated_stripe_s700`) in test mode.
Today's other tests use fakes (`terminal_test.go`, `contract_test.go`); this is
the only path that talks to Stripe itself.

## What runs

`src/simreader_live_test.go` (build tag `stripelive`) calls the production
`terminalCharger.run()`, `refundSale`/`refundLegs`, `readerStates`,
`chooseReader` and `oneReaderState` with a real clock and in-memory storage.
The client and its guards are in `src/livestripe_test.go` (no tag; its
offline unit tests run in every `go test`).

`TestSimulatedS700` (one reader, sequential subtests):

1. `reader_state_ready`: the reader reports `ready`; `chooseReader` accepts it.
2. `sale_approved`: 10.00 sale, default success card; approved with a `pi_...`
   id, PaymentIntent `succeeded`.
3. `refund_partial_legacy`: partial refund by `transaction_ref`; the same call
   again returns the same `re_...` (idempotent replay, ut-docs#6598).
4. `refund_per_leg_rest`: per-leg refund of the rest (`refund_amount`, ut-docs#6603).
5. `refund_over_full_declines`: a further refund declines
   (`charge_already_refunded` or `amount_too_large`).
6. `card_declined_early`: the test-side decorator presents card
   `4000000000000002`; the flow declines with Stripe's reason before the poll
   budget and the PaymentIntent ends `canceled` (the ut-docs#6440 claim).
7. `no_tap_times_out`: the decorator suppresses the tap; `reader_timeout`
   between the poll budget and the hard deadline, PaymentIntent `canceled`.
8. `reader_state_ready_after`: the reader is idle again (`cancel_action` worked).

`TestSimulatedBadKeyIsInvalidKey`: a made-up `sk_test_` key makes
`oneReaderState` report `invalid_secret_key`.

Amounts end in `00` on purpose: other card-present test endings trigger
simulated declines. The currency is the account's `default_currency`.

## Run it locally

```
STRIPE_TEST_TERMINAL_KEY=sk_test_... go test -tags stripelive -v -run '^TestSimulated' ./src/
```

Optional `STRIPE_TEST_TERMINAL_LOCATION=tml_...` picks the location; otherwise
one tagged `metadata[ut_ci]=simreader` is reused or created (kept on purpose,
never deleted; named `test-7081-simulated-reader`, readers `test-7081-simreader-<run>`). Only the reader a run registered is deleted at its end; tagged
leftovers from a killed run are swept at the start of the next run. Do not run
two local runs against one account at once: the sweep can delete the other
run's reader (the CI concurrency group covers CI only). Location reuse scans
only the first 100 locations, so a very large test account may get a new
location per run.

## Safety

- **`sk_test_` only.** `sk_live_`, `rk_live_`, `pk_*`, empty or whitespace keys
  fail before any request. `rk_test_` is refused too: the plugin presents the
  simulated card only when the key starts `sk_test_` (`terminal.go` step 3), so
  with a restricted key the approved sale would silently become a no-tap timeout.
- The test client talks to `https://api.stripe.com` only (a RoundTripper
  refuses any other host).
- `Stripe-Version: 2025-05-28.basil` (first version with
  `simulated_stripe_s700`) is pinned in the **test** client only. Production
  calls still send no version header; pinning there would change behaviour for
  older merchant accounts and is a separate decision.
- Nothing prints the key or Stripe's error `message` (it echoes the key,
  ut-docs#6570): failures show `status=... code=... type=...` only (`no reply (transport error)` when there was no HTTP reply).

## Owner setup (one time)

1. In a Stripe TEST account, create a secret key `sk_test_...`.
2. Settings -> Environments -> new environment `stripe-test`; under
   "Deployment branches" allow `main` only. Add the **environment** secret
   `STRIPE_TEST_TERMINAL_KEY` = that key (not a repo secret, so a dispatch from
   another branch cannot read it). Optional environment variable
   `STRIPE_TEST_TERMINAL_LOCATION` = a `tml_...` id.
3. Actions -> "Simulated reader (Stripe test mode)" -> Run workflow. It also
   runs weekly. Without the secret it ends green with a "skipped" notice. The job also has
`if: github.ref == 'refs/heads/main'`, so a dispatch from another branch skips.

The workflow has no `pull_request` trigger (PR code never sees the secret) and
is not a required check. CI's `validate` job only compiles the tagged file and
checks that it skips.

## What it covers

| Covered | Not covered |
|---|---|
| PaymentIntent create + `process_payment_intent` on a reader id | A real offline reader (simulated readers are always online); offline/404 stay unit-tested with fakes |
| Approved tap; decline by test card number; early decline | Physical PIN entry, soft-decline prompt timing |
| Reader timeout, `cancel_action` and PaymentIntent cancel | Network loss between till and reader |
| Idempotent replay of a refund with the same event id (a sale retry with the same request id is not run live) | The 30 s till gate and the `http_request` host path in the WASM host (this runs native Go) |
| Refunds, legacy and per leg, over-refund decline | The splash logo being displayed (ut-docs#7062) |
| `payment.stripe.readers` state; 401 on a bad key | Tipping, `collect_inputs`, registering a reader from the till (ut-docs#6753) |

## First-run results

TODO (pending the owner's key): registration code accepted (`simulated-s700`),
decline reason seen for card `4000000000000002` (the test accepts only
`generic_decline` or `card_declined`), over-refund decline code, and the reader
state logged right after the approved sale: if it is `busy`, a finished action
keeps the reader busy and `readers.go` needs a production card (treat
`action.status` succeeded/failed as ready).
