# Review: scripts/package.sh counts as shipped (ut-docs#5147)

Date: 2026-10-29. Built by Opus 5.5 and reviewed independently by Sonnet (lane:cloud-41). This change landed the same way in all 19 guarded plugin repos. The shared record is ut-docs `code-reviews/2026-10-29-version-bump-package-sh-5147.md`.

## What shipped
- `scripts/check-version-bump.sh`: `'scripts/package.sh'` is added to `SHIPPED_PATTERNS`. `package.sh` is the packaging recipe. Adding or dropping an `entries` item, or changing the tar flags, changes the release artifact without touching any bundled file. Until now such a PR passed with no version bump and then never released.
- `scripts/check-version-bump.test.sh` gets two new cases:
  - `package.sh` edited with no bump → FAIL, and the output names `scripts/package.sh`;
  - `package.sh` edited with a bump → PASS.
  Where the entries-mirror case rejects patterns it cannot explain, `scripts/package.sh` is added to its allowed list.
- `CLAUDE.md`'s shipped/exempt wording is updated where it had any.
- The shared body is not touched, and its hash is unchanged. No shipped file changed, so there is no version bump.

## Findings
- No blockers and no majors.
- Minor: a review record was needed in every repo. This file is that record.
- Nits accepted:
  - Older dated review records still say "`scripts/` is exempt". They are history, so they stay.
  - The wrapper's long header doesn't mention `package.sh`. The comment on the new line explains it.

## Verified beyond the automated tests
- TDD: restoring the old wrapper makes only the new "package.sh changed, no bump" case fail. This was checked in one repo of each of the 5 test families (tax-uk, tax-de, theme-midnight, language-de, channel-demo). With the change, everything passes.
- `check-version-bump.test.sh` passes in all 19 repos.
- `shellcheck -x` reports no issues.
- No UI surface is touched.

## Verdict
Safe to merge.
