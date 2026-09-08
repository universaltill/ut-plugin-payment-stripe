# Code review: auto-tag-release.yml rollout (ut-docs#1700)

**Date:** 2026-09-08
**Card:** ut-docs#1700 (rollout of ut-docs#1694's workflow to remaining `ut-plugin-*` repos)
**Author:** scrum-master pipeline (cloud cycle, `lane:cloud-41`), on behalf of Pouria Teimouri

## What changed

Added `.github/workflows/auto-tag-release.yml`, copied byte-for-byte from
the canonical, independently-reviewed copy in `ut-plugin-tax-de`
(`docs/code-reviews/2026-09-07-auto-tag-release-workflow-1694.md` there —
full design rationale, recursion-guard analysis and behavioral test
evidence live in that record and are not repeated here per ut-docs#1700's
own instructions; this record only covers what is repo-specific).

## Repo-specific verification

- Confirmed `.github/workflows/release.yml` is tag-triggered
  (`on: push: tags: ["v*"]`) and declares `workflow_dispatch` inputs named
  `channel` (choice, includes `stable`) and `publish` (boolean) — matching
  what the copied workflow's dispatch step passes, with no adaptation
  needed.
- `diff` against the canonical `ut-plugin-tax-de` copy: byte-identical.
- **Live drift this repo was actually in** (ut-docs#1756: "main has shipped
  v1.1.0 content since 2026-08-01 with no release tag — real installs still
  on v1.0.0"; ut-docs#1772 names this repo's own stuck `v1.2.1` tag): at the
  time this PR was opened, `manifest.json`'s `version` was `1.2.1` but the
  latest tag on `main` was only `v1.2.0`. Merging this workflow is expected
  to create tag `v1.2.1` and dispatch a real `release.yml` run on the first
  push to `main` that carries it — this closes ut-docs#1772's stuck-tag gap
  for this repo as a side effect. DevOps must verify the tag and release
  run actually happened rather than trust the YAML.
- `manifest.json` confirmed at repo root (script's assumption).

## Independent review

Mechanically identical to the already-reviewed canonical file (full
independent Opus review on the original: ut-plugin-tax-de's own
2026-09-07 record). This repo's own diff is a verbatim copy plus this
review record — verdict: **SAFE TO MERGE**, no repo-specific deviation
found.
