# Review: reader_id per call + readers state hook (ut-docs#6566), Stripe v1.9.0

Card: universaltill/ut-docs#6566 (plugin part, PR 2 of the architect's order). Design: `/tmp/tabletfix/pm-architect.md` section 4. Status: Dev done; combined Tester PASS and independent Reviewer done 2026-11-19 (sections at the end).

## What shipped
- `src/readers.go`: `validReaderID` (`tmr_` + 1..64 alphanumerics), `chooseReader`, `readerStates`, `readersAnswer`.
- `src/main.go`: authorize uses the payload `reader_id`, else `stripe_reader_id`; a malformed payload id declines `invalid_reader` (no API call, no fallback). New `payment.stripe.readers` hook, read-only, exit 0.
- `src/host.go`: `reader_id`/`reader_kind`/`op`/`readers` on the event payload; `reader_id` on `original_payments[]` (accepted, never used).
- Manifest 1.8.0 to 1.9.0, hook `payment.stripe.readers`; README and CLAUDE.md updated.

## Checks to re-verify
- Malformed ids (`tmr_1/../x`, `../`, 65 chars, wrong prefix, whitespace, newline, `%2f`) decline with zero HTTP calls (`TestChooseReaderRejectsMalformedNoFallback`, `TestReaderStatesTruncatesAndNeverCallsMalformed`).
- Existing contract, terminal and refund tests are unchanged and green.
- Refund with a malformed `reader_id` still refunds by `transaction_ref` (`TestRefundIgnoresReaderIDOnLegs`).
- Logs carry only `maskRef(reader)` and never Stripe's message (`TestReaderStateLogsMaskedOnly`).
- Money: nothing touched; amounts stay integer minor units.

## Not verified
No live or simulated reader: the state mapping follows Stripe's documented `GET /v1/terminal/readers/{id}` shape (`status`, `action`) against fakes only.

## Tester (combined pass, 2026-11-19)
PASS. gofmt, `go vet` (host and `GOOS=wasip1 GOARCH=wasm`), `go test -race ./...`, `scripts/build.sh`, `validate.sh`, `guard-plugin-i18n.sh`, `package.sh`, `check-version-bump.test.sh` green. Two mutants killed (format check made permissive; invalid id falling back to the setting).

## Reviewer (independent, 2026-11-19)
Reviewed by: Fable 5.2 (independent Reviewer agent; the code was written by a Sonnet Dev agent). One combined review for ut-docs#6566 across core, both plugins and ut-docs.

Re-run personally: `go test -count=1 ./...` ok; `GOOS=wasip1 GOARCH=wasm go vet ./...` clean. Read in full: `src/readers.go`, the `main.go`/`host.go` diff, the reader id's path into every provider URL.

Findings:
- None blocking. No change made to this repo by the review.
- L1 (accepted, pre-existing): the legacy `stripe_reader_id` setting is still put into the provider URL path without a format check (only `TrimSpace`) when the till sends no `reader_id`. Unchanged behaviour, manager-entered value; core now refuses to import a malformed one into the registry. Follow-up card, not this PR.
- L2 (accepted): the `main.go` wiring (the `decline(invalid_reader)` call and the `.readers` exit path) is `wasip1`-tagged, so `go test` does not execute it; it was read line by line, and `chooseReader` / `readerStates` behind it are unit-tested. A sandbox or simulated-reader run of the packaged wasm is still owed before the release tag.
- Checked and fine: a refund never reaches `chooseReader` (`refundCharge` returns first), so core's top-level `reader_id` on a reversal cannot decline a refund; the `.readers` branch runs before any key check and always exits 0; the hook is read-only (GET only); per-request plugin state is keyed by the till's request id, so two readers never share an attempt record; an old till (no `reader_id`) behaves exactly as before.

Security exposure (what this change exposes):
- New input: a reader id supplied per call by the till. It is format-checked (`tmr_` + 1..64 letters/digits) before any URL is built; a malformed one declines `invalid_reader` with zero provider calls and no fallback to another reader. No new permission, no new host function, no new secret; the manifest only gains the `payment.stripe.readers` hook.
- New outbound call: `GET` reader status, at most 16 per `.readers` call, only for ids that pass the format check.
- Logs: reader ids masked to the last 4 characters; the provider's error text is never logged on 401. No card data anywhere.

Verdict: ready to commit and open the PR. Safe to release alone (an older core never fires the hook and never sends `reader_id`). Release after the ut-docs amendments PR; multi-reader only becomes active once the core PR is released too.
