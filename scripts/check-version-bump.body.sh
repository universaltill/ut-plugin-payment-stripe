#!/usr/bin/env bash
# shellcheck shell=bash
#
# Shared body of scripts/check-version-bump.sh (ut-docs#4489; the guard itself
# is ut-docs#1940, rolled out per ut-docs#1948).
#
# This file is SOURCED by scripts/check-version-bump.sh, after that wrapper
# has `cd`-ed to the repo root and set SHIPPED_PATTERNS (and optionally
# SHIPPED_NOTE, the kind-specific "what the release artifact bundles" text).
# It is byte-identical in every ut-plugin-* repo. The canonical copy lives in
# ut-docs at scripts/templates/check-version-bump.body.sh -- NEVER edit a
# plugin's copy: edit the canonical one, then run
#   scripts/templates/sync-version-bump-body.sh --rehash
#   scripts/templates/sync-version-bump-body.sh <plugin-repo-dir>...
# (ut-docs#4489). The self-check below fails CI on any hand edit.
#
# body-sha256 is the sha256 of this file with the body-sha256 line removed.
# body-sha256: 1875002a8cb6a8a5f50552a4f7457041d1df4896373d00d65b4b9a38bbf0e658

_vb_hash_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        grep -v '^# body-sha256: ' "$1" | sha256sum | cut -d' ' -f1
    else
        grep -v '^# body-sha256: ' "$1" | shasum -a 256 | cut -d' ' -f1
    fi
}
_vb_recorded=$(sed -n 's/^# body-sha256: //p' "${BASH_SOURCE[0]}" | head -n 1)
if [ "$(_vb_hash_of "${BASH_SOURCE[0]}")" != "$_vb_recorded" ]; then
    echo "FAIL: scripts/check-version-bump.body.sh differs from the canonical copy (ut-docs scripts/templates/check-version-bump.body.sh) -- edit that copy and run its sync script, never this file (ut-docs#4489)" >&2
    exit 1
fi
unset -f _vb_hash_of
unset _vb_recorded

# BASE_SHA/HEAD_SHA follow the same convention as
# .github/workflows/commit-attribution.yml: passed in by the workflow from
# the pull_request event (github.event.pull_request.base.sha / head.sha).
# For a local/manual run, fall back to origin/main's tip as BASE_SHA (not
# `git merge-base HEAD origin/main`: MERGE_BASE is computed from BASE_SHA
# below, and a merge-base here would make BASE_SHA == MERGE_BASE locally, so
# the "forked before manifest.json existed" check could never fire).
BASE_SHA="${BASE_SHA:-}"
HEAD_SHA="${HEAD_SHA:-}"
if [ -z "$BASE_SHA" ]; then
    BASE_SHA=$(git rev-parse origin/main 2>/dev/null || true)
fi
if [ -z "$HEAD_SHA" ]; then
    HEAD_SHA=$(git rev-parse HEAD)
fi
if [ -z "$BASE_SHA" ]; then
    echo "ERROR: could not determine BASE_SHA (set BASE_SHA explicitly, or ensure origin/main is fetched)"
    exit 1
fi

# github.event.pull_request.base.sha is the BASE BRANCH'S TIP at event time,
# not this PR's merge-base -- it advances as main advances. Diffing/reading
# directly against BASE_SHA (two-dot) leaks main's own later commits into
# "what this PR changed", in both directions: once ANY other shipped PR
# merges and bumps the version, every other still-open shipped-file PR
# would start comparing against that higher version and pass unbumped
# (false negative -- exactly the multi-lane scenario in the wrapper's header);
# and a docs-only PR whose base.sha happens to sit after an unbumped
# shipped change on main would be told to bump for someone else's change
# (false positive). Compare against the merge-base instead, so only what
# THIS PR itself introduced is ever in scope.
MERGE_BASE=$(git merge-base "$BASE_SHA" "$HEAD_SHA") || {
    echo "ERROR: could not compute merge-base of ${BASE_SHA} and ${HEAD_SHA}"
    exit 1
}

# core.quotePath=false so a shipped path with non-ASCII characters is
# printed as a real UTF-8 path, not C-style-quoted octal escapes that would
# never match SHIPPED_PATTERNS.
#
# --no-renames because rename detection (on by default since git 2.9) prints
# only the DESTINATION path for a renamed file, which hides every rename OUT
# of a shipped location: `git mv src/app/f.go docs/f.go` (or
# `git mv locales/de.json docs/de.json`) removes a file from the release
# artifact but reports as a lone `docs/...` path, and the guard would answer
# "no shipped file changed" (verified as a real false negative in the
# compiled variant's 2026-09-10 review). With --no-renames the same change
# reports as a delete + an add, so the shipped side is seen. Renames INTO or
# WITHIN a shipped location were already caught either way (the destination
# is the shipped path), so this only ever adds coverage.
changed_files=$(git -c core.quotePath=false diff --no-renames --name-only "$MERGE_BASE" "$HEAD_SHA" -- .)

shipped_changed=()
while IFS= read -r f; do
    [ -n "$f" ] || continue
    for pat in "${SHIPPED_PATTERNS[@]}"; do
        # Intentional unquoted glob match ($pat is a pattern, not a literal).
        # shellcheck disable=SC2053
        if [[ "$f" == $pat ]]; then
            shipped_changed+=("$f")
            break
        fi
    done
done <<<"$changed_files"

if [ "${#shipped_changed[@]}" -eq 0 ]; then
    echo "ok: no shipped file changed (${MERGE_BASE:0:9}..${HEAD_SHA:0:9}) -- no version bump required"
    exit 0
fi

read_version() {
    # $1 = git ref. Fails closed (non-empty error) if manifest.json is
    # missing or doesn't parse at that ref, rather than silently treating
    # it as "no version".
    git show "$1:manifest.json" 2>/dev/null | python3 -c '
import json, sys
try:
    print(json.load(sys.stdin)["version"])
except Exception as e:
    print(f"ERROR:{e}", file=sys.stderr)
    sys.exit(1)
'
}

# A new repo's first PR (ut-docs#2964, ut-docs#4484): the merge-base has no
# manifest.json at all (the repo started with a licence/review-record
# commit), so there is no earlier version to bump from -- a first release,
# not a missing bump. Only a MISSING file takes this path; one that exists
# but doesn't parse still fails closed below, and the already-tagged check
# further down still applies, so a first version that was somehow released
# stays refused. main's tip must lack it too: a branch forked before the
# first manifest landed would otherwise merge an already-shipped version
# that never releases. (cat-file can't tell "absent" from "object missing",
# but CI fetches full history and merge-base above already failed closed if
# not.) This whole file is identical in every ut-plugin-* copy (ut-docs#4489).
first_release=0
if ! git cat-file -e "${MERGE_BASE}:manifest.json" 2>/dev/null; then
    if git cat-file -e "${BASE_SHA}:manifest.json" 2>/dev/null; then
        echo "FAIL: this branch forked before manifest.json existed, but ${BASE_SHA:0:9} already has one -- rebase onto it and bump the version"
        exit 1
    fi
    if ! git cat-file -e "${HEAD_SHA}:manifest.json" 2>/dev/null; then
        echo "ok: no manifest.json at merge-base ${MERGE_BASE:0:9} or head ${HEAD_SHA:0:9} -- nothing releases yet"
        exit 0
    fi
    first_release=1
    base_version="(none: first release)"
else
    base_version=$(read_version "$MERGE_BASE") || {
        echo "ERROR: could not read manifest.json version at merge-base ${MERGE_BASE:0:9}"
        exit 1
    }
fi
head_version=$(read_version "$HEAD_SHA") || {
    echo "ERROR: could not read manifest.json version at head ${HEAD_SHA:0:9}"
    exit 1
}

fail_no_bump() {
    # $1 = the specific reason line (already ends without a period).
    # SHIPPED_NOTE (set by the wrapper) says what package.sh bundles.
    {
        echo "FAIL: ${1}"
        echo ""
        echo "Changed shipped file(s):"
        printf '  - %s\n' "${shipped_changed[@]}"
        echo ""
        echo "Why this fails the build: scripts/package.sh bundles ${SHIPPED_NOTE:-these files}"
        echo "into the release artifact, and auto-tag-release.yml only cuts a release when"
        echo "manifest.json's version differs from the last tag. Without a genuinely new"
        echo "version, this change lands on main and then SILENTLY NEVER SHIPS -- the"
        echo "marketplace keeps serving the old artifact with no failing signal anywhere"
        echo "(ut-docs#1940, ut-docs#4489)."
        echo ""
        echo "Fix: bump manifest.json's \"version\" in this PR to ${2}."
    } >&2
    exit 1
}

# Suggest a patch bump as a starting point -- the author picks minor/major
# if the change actually warrants it. Only offered when every segment is a
# plain integer (semver's optional -prerelease/+build metadata, e.g.
# "1.0.0-beta.1", makes `patch` a non-integer string like "0-beta.1" and
# must never be allowed to reach arithmetic -- a version scheme this repo's
# own validate.sh/auto-tag-release.yml already accept must not silently
# defeat this guard).
IFS='.' read -r major minor patch <<<"$head_version"
if [[ "$major" =~ ^[0-9]+$ && "$minor" =~ ^[0-9]+$ && "$patch" =~ ^[0-9]+$ ]]; then
    suggested="a version higher than ${head_version}, e.g. \"${major}.${minor}.$((patch + 1))\""
else
    suggested="a version higher than ${head_version}"
fi

if [ "$first_release" -eq 0 ] && [ "$base_version" = "$head_version" ]; then
    fail_no_bump "shipped file(s) changed but manifest.json's version is still ${head_version}" "$suggested"
fi

# The version differs from this PR's own base -- but a version that was
# already released elsewhere is just as silent a failure: auto-tag-
# release.yml sees the tag already exists and treats it as nothing to do.
# fetch-depth: 0 in the workflow fetches tags along with full history.
if git rev-parse -q --verify "refs/tags/v${head_version}" >/dev/null 2>&1; then
    fail_no_bump "manifest.json's version changed to ${head_version}, but v${head_version} is ALREADY TAGGED (released)" "$suggested"
fi

echo "ok: manifest.json version bumped ${base_version} -> ${head_version}, covering:"
printf '  - %s\n' "${shipped_changed[@]}"
