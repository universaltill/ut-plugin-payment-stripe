# Review — simulated S700 reader live test path (ut-docs#7081)

Date: 2026-11-26 · Lane: `lane:cloud-41` (routine `:41`) · Branch: `test/7081-simulated-reader` · Version 1.10.1

## What shipped

- `src/livestripe_test.go` (untagged, runs in every `go test`) holds the live test client and its guards:
  - Only `sk_test_` keys are accepted. `rk_test_` is refused because the flow presents a card only for `sk_test_`.
  - Requests may go to `https://api.stripe.com` only (`stripeHostOnly`).
  - `Stripe-Version: 2025-05-28.basil` is pinned in the test client only.
  - Failures print `errSummary`, never Stripe's message or the key.
  - A test-side `presentOverride` produces the decline and no-tap cases.
  - Offline unit tests pin each guard.
- `src/simreader_live_test.go` (build tag `stripelive`, env `STRIPE_TEST_TERMINAL_KEY`) registers a `simulated-s700` reader in a test location and deletes it again. It drives the real `terminalCharger.run()`, `refundSale`/`refundLegs` and `readerStates` through these scenarios:
  - reader ready
  - approved sale
  - partial refund, then its idempotent replay
  - per-leg refund of the rest
  - over-refund declined
  - early card decline
  - no tap ending in `reader_timeout`
  - reader ready again
  - a bad key ending in `invalid_secret_key`
- `.github/workflows/simulated-reader.yml` runs on dispatch and a weekly schedule only, never on a PR. It runs on `main` only, in environment `stripe-test`, with `contents: read`. It skips green with a notice until the owner adds the secret, and fails on a non-`sk_test_` secret.
- `ci.yml` vets the tagged file and checks that it skips, so it can't rot.
- Docs: new `docs/simulated-reader.md` (what the simulator can and cannot cover, owner setup, first-run results to fill in). Pointers added in CLAUDE.md, README and the `contract_test.go` header, and in ut-docs `reference/provider-sandboxes.md`.
- No production code changed.

## Phases

- BA (Opus): Stripe facts came from Stripe's OpenAPI spec and the stripe-node changelog, because docs.stripe.com is blocked in this session.
- Architect (Opus). Dev (Sonnet), plus two fix rounds (Sonnet).
- Tester: gate green. 21 of 25 mutants were caught in a scratch copy. The gap it found (T1: the host guard could be unwired with no test failing) is fixed.
- Independent review: Opus 5.5, a different model from the Sonnet author.

## Findings

| # | Sev | Finding | Outcome |
|---|---|---|---|
| T1 | medium | Dropping `stripeHostOnly` from the client failed no test; a path of `@evil.example/x` would send the key elsewhere | fixed: `TestLiveStripeForwardsNothingWhenPathRedirectsHost`. The orchestrator checked it fails with the guard removed and passes with it restored |
| T2 | low | Host table missed suffix/prefix tricks | fixed |
| D1 | low | Docs claimed a sale retry was covered | fixed: only the refund replay is claimed |
| L1(t) | low | Test data naming | fixed: `test-7081-…` |
| L2(t) | low | A failed sale skipped independent subtests | fixed: only the refund steps skip |
| L3/L4(t) | nit | Raw form body; ignored DELETE replies | fixed |
| M1 | medium | `workflow_dispatch` from any branch could read the repo secret | fixed: `if: github.ref == 'refs/heads/main'` and environment `stripe-test` (deployment branch `main`) |
| L1 | low | `readers.go` may report a reader busy after a finished action | logged after the sale; recorded in First-run results; a card is filed if the first run confirms it |
| L2 | low | Decline assertion was only an exclusion list | fixed: `generic_decline`/`card_declined` |
| L3 | low | 10 s client timeout vs the 34 s no-tap bound | fixed: 5 s |
| N1/N2/N4 | nit | Real company address; empty `errSummary` on no reply; cleanup wording | fixed |
| N3 | nit | Location scan stops at 100; a concurrent local run's reader can be swept | accepted, documented as known limits |
| N5 | nit | ci.yml has no top-level `permissions:` | accepted, pre-existing |

## Verification

- `gofmt -l .` is empty.
- These all pass: `GOOS=wasip1 GOARCH=wasm go vet ./...`, `go vet -tags stripelive ./...`, `go test -count=1 ./...`, `scripts/build.sh`, `scripts/validate.sh` (v1.10.1), `scripts/package.sh`, `scripts/guard-plugin-i18n.sh`.
- `go test -tags stripelive -run Simulated` skips both tests.
- With fake keys, `rk_test_`/`sk_live_`/`pk_test_` fail before any request and the value is never printed.
- The version-bump guard reports 1.10.0 → 1.10.1.
- **Not verified:** a real run against Stripe test mode. There is no key, and api.stripe.com is unreachable here. Unverified until that run: the `simulated-s700` registration code, the minimum API version, and the exact decline and over-refund codes.
- actionlint is not installed; the workflow YAML was parsed with Python.

Pre-flight: plan/price n/a; platform claim n/a (the README "Testing" section is not a capability claim); till behaviour, help and docs-shots n/a (no production code); en.json n/a; my./partner./admin UI n/a; ADR n/a (test-only, Stripe-Version pinned in the test client only); plugin type/host fn/manifest field n/a (version value only); reference page updated (ut-docs `reference/provider-sandboxes.md`); review record: this file.

## Verdict

Ship. Deferred: the first live run. The owner creates environment `stripe-test` with deployment branch `main` and secret `STRIPE_TEST_TERMINAL_KEY` (a Stripe **test** `sk_test_` key), then runs the "Simulated reader" workflow and records the results in `docs/simulated-reader.md`. Tracked on ut-docs#7081.
