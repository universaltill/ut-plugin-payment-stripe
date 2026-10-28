# Review: version-bump guard rollout (ut-docs#4917)

Date: 2026-10-28. Built by Sonnet and reviewed independently by Opus 5.5 (lane:cloud-41).

## What shipped
This PR adds the manifest.json version-bump guard (ut-docs#1940) to this repo. `auto-tag-release.yml` releases only when the version changes, so a shipped change without a bump used to land on main and silently never release.
- `scripts/check-version-bump.sh` is the wrapper. Variant: **compiled**. `SHIPPED_PATTERNS`: manifest.json, README.md, LICENSE, src/*, go.mod, go.sum, scripts/build.sh. The list mirrors `scripts/package.sh`, including its conditional entries, plus the inputs `build.sh` compiles into `bin/plugin.wasm` where that applies.
- `scripts/check-version-bump.body.sh` is synced byte-for-byte from ut-docs `scripts/templates/` with `sync-version-bump-body.sh` (ut-docs#4489). It checks its own hash, so a hand edit fails CI.
- `scripts/check-version-bump.test.sh` runs fixture cases in a `mktemp` git sandbox. It includes an "entries mirror" case that re-reads this repo's `package.sh`/`build.sh`, so drift between them and the patterns fails.
- `ci.yml` gets a `version-bump` job, copied from ut-plugin-tax-de: PR-only, `fetch-depth: 0`, with `BASE_SHA`/`HEAD_SHA` taken from the pull_request event.
- No shipped file changed, so there is no version bump.

## Findings (from the review across all 8 repos)
- No blockers.
- M1, m1, m3 and n1 were in ut-docs (cycle-start step 6 and the doc sentence). All are fixed there.
- m2 is a design gap that predates this change, in all 17 repos: `scripts/package.sh` is not itself a shipped pattern. It is filed as ut-docs#5147.

## Verified beyond the automated tests
- Break-it checks: each one made the test fail, and restoring the line made it pass again.
  - Dropping a pattern from the wrapper failed both that pattern's case and the entries-mirror case. The orchestrator re-ran this independently on tax-uk: removing `locales/*` gave 17 FAILs.
  - A hand edit of the body failed its hash self-check.
- Throwaway clone: with this change committed, the guard prints "ok: no shipped file changed". Adding a README.md edit makes it fail with "version is still X".
- `shellcheck scripts/check-version-bump*.sh`: 0 issues.
- `sync-version-bump-body.sh --check`: ok.
- No UI surface is touched.

## Verdict
Safe to merge.
