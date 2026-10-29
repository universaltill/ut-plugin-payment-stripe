#!/usr/bin/env bash
#
# Requires a manifest.json version bump on any PR that touches a shipped file
# (ut-docs#1940, rolled out here per ut-docs#4917).
#
# WHY THIS EXISTS: scripts/package.sh bundles manifest.json, README.md,
# LICENSE (when present) and bin/ (the compiled WASI module) into the
# marketplace release artifact. None of this repo's other checks (validate,
# package dry run, authors) look at the version field, so a PR that changes
# what ships but forgets to bump manifest.json's version lands on main,
# passes every check, and then SILENTLY NEVER SHIPS: auto-tag-release.yml
# reads the unchanged version, sees v<version> already tagged/released, and
# correctly does nothing. main and the marketplace diverge with no failing
# signal anywhere (ut-docs#1940).
#
# WHAT COUNTS AS "SHIPPED" -- THE COMPILED-PLUGIN VARIANT: this repo builds a
# WASI module (scripts/build.sh: `go build -o bin/plugin.wasm ./src`) and
# package.sh's entries=(...) line bundles that compiled `bin` directory.
# `bin/` is gitignored, so it can NEVER appear in `git diff --name-only` no
# matter how much the plugin's behaviour changed; keying SHIPPED_PATTERNS off
# the literal bundled path would silently never fire. So SHIPPED_PATTERNS
# also covers the SOURCE that PRODUCES bin/plugin.wasm: every file under src/
# (the `go build ... ./src` argument), go.mod/go.sum (a dependency bump
# changes the compiled binary without touching anything under src/), and
# scripts/build.sh itself -- the BUILD RECIPE, so a new
# -ldflags/-trimpath/-tags or a different GOOS/GOARCH changes the shipped
# binary while every file under src/ stays byte-identical.
#
# One accepted, deliberate over-approximation: `src/*` also matches
# `*_test.go` files, which go build never compiles into bin/plugin.wasm -- a
# test-only change will ask for a version bump it does not strictly need.
# That is a false positive (harmless extra bump), never a false negative (a
# real behaviour change shipping unbumped).
#
# Keep this list mirrored to package.sh's own entries (`entries=(...)` and
# every `entries+=(...)` line) and build.sh's own `go build` source argument
# -- nothing enforces that automatically, so a change to either needs a
# matching edit here (check-version-bump.test.sh's "entries mirror" case
# re-reads package.sh's and build.sh's own source and fails on drift in
# either direction). The check logic itself lives in
# scripts/check-version-bump.body.sh, sourced at the bottom: one canonical
# body, byte-identical in every ut-plugin-* repo (canonical copy: ut-docs
# scripts/templates/check-version-bump.body.sh, ut-docs#4489). Never edit the
# body here; only SHIPPED_PATTERNS (and SHIPPED_NOTE) are per-repo.
set -euo pipefail
cd "$(dirname "$0")/.."

# SHIPPED_PATTERNS is read by the sourced body, which shellcheck does not
# follow without -x.
# shellcheck disable=SC2034
SHIPPED_PATTERNS=(
    'manifest.json'
    'README.md'
    'LICENSE'
    'src/*'
    'go.mod'
    'go.sum'
    'scripts/build.sh'
    # The packaging recipe: it decides what goes into the release artifact
    # (entries, tar flags), so an edit to it changes what ships (ut-docs#5147).
    'scripts/package.sh'
)

# What package.sh bundles, for the failure text in the shared body.
# shellcheck disable=SC2034
SHIPPED_NOTE="these files (or, for src/**/go.mod/go.sum, the compiled bin/plugin.wasm they produce)"

# shellcheck source=scripts/check-version-bump.body.sh
. scripts/check-version-bump.body.sh
