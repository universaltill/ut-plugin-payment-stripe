# Code review: propagate ut-docs#166 release-pipeline template fixes

**Date:** 2026-08-24
**Card:** ut-docs#497
**Author:** scrum-master pipeline (cloud cycle), on behalf of Farshid Mirza

## What changed

Applied the same fixes verified in ut-docs#166's review to this repo's copy
of the shared release-pipeline template (`scripts/package.sh`,
`.github/workflows/release.yml`, `scripts/approve.sh`, `.github/workflows/ci.yml`):

1. **Checksum content mismatch** — `scripts/package.sh` and the `release.yml`
   "Create GitHub Release" step both recorded a `dist/`-prefixed path inside
   the `.sha256` sidecar instead of the bare filename, breaking
   `sha256sum -c` for self-hosters who download the artifact + checksum pair
   into one directory. Fixed by `cd`-ing into `dist/` before hashing
   (`package.sh`) and regenerating (not `cp`-ing) the `latest.tar.gz.sha256`
   sidecar against the renamed file (`release.yml`).
2. **Admin/vendor token conflation** — `scripts/approve.sh` used the
   vendor-upload token (`MARKETPLACE_UPLOAD_TOKEN`) as the admin-endpoint
   bearer token. Client-side fix (matching #166): prefer a distinct
   `MARKETPLACE_ADMIN_TOKEN` when set, falling back to the upload token
   otherwise — no behavior change while the repo secret stays unset. The
   `release.yml` "Auto-approve" step's `env:` block now threads
   `MARKETPLACE_ADMIN_TOKEN` through alongside the existing upload token.
3. **No `go vet`/`gofmt` gate** — `.github/workflows/ci.yml` now runs
   `GOOS=wasip1 GOARCH=wasm go vet ./...` (cross-compiled, since the
   `//go:build wasip1`-tagged entry point makes host `go vet` silently skip
   the real code) and a `gofmt -l .` check, matching
   `ut-plugin-payment-sumup`'s CI.

## Why no re-litigation

This is a mechanical propagation pass, not new design — ut-docs#166's own
review already validated this exact fix pattern (confirmed against the
now-fixed `ut-plugin-faq` and `ut-plugin-payment-sumup` as the reference
implementations, byte-for-byte identical diff). ut-docs#497's own text calls
this out as a non-goal: "Re-litigating the fix approach — #166's review
already validated it."

## Verification performed (this repo)

- `GOOS=wasip1 GOARCH=wasm go vet ./...`: clean, no findings.
- `gofmt -l .`: clean, no output.
- `go test ./...` (host-buildable packages): all pass.
- `scripts/build.sh`: WASI module builds successfully.
- `scripts/package.sh` (which calls `scripts/validate.sh`): passes, artifact
  packaged.
- `sha256sum -c` against the freshly generated `.sha256` sidecar: **OK** —
  confirms the checksum fix actually produces a verifiable pair, not just
  parses.
- `git diff --stat` confirms only the intended files changed; build-artifact
  noise from the dry-run build/package step was reverted before commit.


## Scope note

ut-docs#497 covers ~11 remaining `ut-plugin-*` repos serving a release
artifact. This PR is one of that batch; each repo gets its own PR (matching
the ecosystem's per-repo review/commit convention) and its own review
record, since the diff and verification are identical in kind across all of
them. The closing comment on ut-docs#497 lists every repo checked and its
outcome.

## Non-goals confirmed out of scope

- Building the real distinct admin-token support in ut-cloud (tracked
  separately per #497).

## Independent review (fresh-context Opus, reviewing this exact batch)

The Opus review traced `approve.sh`'s token-fallback logic through all 6
env-var permutations by execution (including the production-relevant case:
GitHub Actions renders an unset secret as an empty string, so the code's
`-z` value-test — not a `-v`/`+set` variable-test — is what makes the
fallback actually fire; a `+set` test would have silently 401'd every
plugin's auto-approve step in production), re-derived the checksum fix
end-to-end (reproduced the original bug, confirmed both sidecars now name
the bare filename, confirmed `sha256sum -c` passes), confirmed redirection
placement and copy-before-hash ordering are correct in every repo, and
confirmed `scripts/publish.sh`'s only other consumer of the sidecar
(`cut -d' ' -f1`) is unaffected by the filename fix. Verdict: PASS on
mechanics in all 11 repos, no repo missing a fix it should have.

It found two real defects, both **factual errors in comments**, not logic
bugs — no `run:` command changed as a result of either fix below.

**Finding 1 (merge-blocking, fixed):** the `release.yml` "Auto-approve"
step's comment claimed "ut-cloud doesn't accept a distinct admin credential
yet" — **false**. Verified directly against `ut-cloud`'s own source
(`internal/config/config.go`, `internal/api/claims_pages.go`,
`internal/httpapi/router/router.go`): `authorizeStaff` has honored
`MARKETPLACE_ADMIN_TOKEN` exclusively, once configured, since ut-docs#496 —
already shipped and documented. The comment (copied verbatim from the
reference repos, where it was accurate at #166's original time) had gone
stale and, worse, directly contradicted `approve.sh`'s own comment in the
same commit, which had the correct version. Fixed: the `release.yml`
comment now matches reality and no longer tells operators the secret can't
do anything yet — it correctly frames provisioning the actual secret value
as a separate ops action, not a code gap.

**Finding 2 checked, not applicable here:** this repo's `src/main.go`
*does* carry the `//go:build wasip1` tag the `go vet` comment describes, so
host `go vet ./...` genuinely would silently skip it — the original comment
is accurate for this repo and was left unchanged.
