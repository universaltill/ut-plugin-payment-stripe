#!/usr/bin/env bash
# Tests for scripts/check-version-bump.sh (ut-docs#1940, rolled out here per
# ut-docs#4917).
#
# Each case builds a throwaway git repo in a mktemp sandbox (real commits, so
# BASE_SHA/HEAD_SHA and `git diff`/`git show` behave exactly as they do against
# a real PR) with just enough of this repo's layout for the script under test
# to operate on, then runs the real script against a BASE_SHA..HEAD_SHA pair
# and asserts both the exit code and that the failure output actually names
# the changed shipped file(s) and the version -- a script that exits non-zero
# for the wrong reason must not be mistaken for a passing test of the real
# check. Nothing here writes into the real tree.
#
# This fixture mirrors the COMPILED-plugin shape (WASI module built from src/
# via go build). bin/ is gitignored, so it can never appear in `git diff --name-only`;
# the guard keys off the source that produces it (src/, go.mod, go.sum and
# scripts/build.sh) instead.
# See check-version-bump.sh's own header.
set -euo pipefail
cd "$(dirname "$0")/.."

REAL_SCRIPT="$(pwd)/scripts/check-version-bump.sh"
REAL_BODY="$(pwd)/scripts/check-version-bump.body.sh"
PACKAGE_SH="$(pwd)/scripts/package.sh"
BUILD_SH="$(pwd)/scripts/build.sh"
REAL_ROOT="$(pwd)"
FAILS=0

# Bundled directories / extra top-level files (beyond manifest.json,
# README.md and LICENSE) that the fixture models; they mirror this repo's
# package.sh. The "entries mirror" case below re-reads the REAL package.sh,
# so this list cannot hide drift -- it only decides which fixture files exist.
EXTRA_DIRS=()
EXTRA_FILES=()

work_dir=""
cleanup() {
    # `|| true`: under `set -e`, a trap function's own exit status can
    # become the script's final exit status -- without this, a clean run
    # that fires EXIT while work_dir is still "" would report failure.
    [ -n "$work_dir" ] && rm -rf "$work_dir"
    true
}
trap cleanup EXIT

# bump_version <new-version> -- rewrites manifest.json's version in $case_dir.
bump_version() {
    python3 -c "
import json
m = json.load(open('${case_dir}/manifest.json'))
m['version'] = '$1'
json.dump(m, open('${case_dir}/manifest.json', 'w'))
"
}

# fresh_repo
# Builds a throwaway git repo ($case_dir) with an initial commit carrying a
# minimal but complete layout (manifest.json v0.1.0, README.md, LICENSE,
# CLAUDE.md (not shipped), a workflow file, a docs file, a sample file in each
# bundled dir, src/, go.mod, go.sum, build.sh). Sets $base_sha to that commit. The
# script under test is copied in (not symlinked -- it does
# `cd "$(dirname "$0")/.."`, which resolves relative to ITS OWN path, so it
# must run from inside the fixture).
fresh_repo() {
    [ -n "$work_dir" ] && rm -rf "$work_dir"
    work_dir="$(mktemp -d)"
    case_dir="${work_dir}/repo"
    mkdir -p "${case_dir}/scripts" "${case_dir}/.github/workflows" "${case_dir}/docs/code-reviews"
    cp "$REAL_SCRIPT" "${case_dir}/scripts/check-version-bump.sh"
    cp "$REAL_BODY" "${case_dir}/scripts/check-version-bump.body.sh"
    chmod +x "${case_dir}/scripts/check-version-bump.sh"

    cat >"${case_dir}/manifest.json" <<'JSON'
{
  "id": "com.universaltill.payment-stripe",
  "name": "Fixture Plugin",
  "version": "0.1.0",
  "runtime": "wasm"
}
JSON
    echo "# Fixture plugin" >"${case_dir}/README.md"
    echo "MIT" >"${case_dir}/LICENSE"
    echo "# agent rules (not shipped)" >"${case_dir}/CLAUDE.md"
    echo "name: CI" >"${case_dir}/.github/workflows/ci.yml"
    local d f
    for d in ${EXTRA_DIRS[@]+"${EXTRA_DIRS[@]}"}; do
        mkdir -p "${case_dir}/${d}"
        echo "sample" >"${case_dir}/${d}/sample.txt"
    done
    for f in ${EXTRA_FILES[@]+"${EXTRA_FILES[@]}"}; do
        echo "# ${f}" >"${case_dir}/${f}"
    done
    mkdir -p "${case_dir}/src"
    printf 'package main\n\nfunc main() {}\n' >"${case_dir}/src/main.go"
    cat >"${case_dir}/go.mod" <<'GOMOD'
module github.com/universaltill/fixture

go 1.23
GOMOD
    echo "" >"${case_dir}/go.sum"
    cat >"${case_dir}/scripts/build.sh" <<'BUILD'
#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
GOOS=wasip1 GOARCH=wasm go build -o bin/plugin.wasm ./src
BUILD
    printf '/bin/\n/dist/\n' >"${case_dir}/.gitignore"

    (
        cd "$case_dir"
        git init -q
        git config user.email test@example.com
        git config user.name Test
        git add -A
        git commit -q -m "initial"
    )
    base_sha=$(cd "$case_dir" && git rev-parse HEAD)
}

# commit_change - stages whatever the caller already edited in $case_dir
# and commits it, setting $head_sha.
commit_change() {
    (cd "$case_dir" && git add -A && git commit -q -m "change")
    head_sha=$(cd "$case_dir" && git rev-parse HEAD)
}

run_check() {
    (cd "$case_dir" && BASE_SHA="$base_sha" HEAD_SHA="$head_sha" bash scripts/check-version-bump.sh)
}

assert_pass() {
    local name="$1"
    local out rc
    set +e
    out="$(run_check 2>&1)"
    rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
        echo "FAIL [$name]: expected exit 0, got $rc. Output:"
        echo "$out"
        FAILS=$((FAILS + 1))
        return
    fi
    echo "ok   [$name]"
}

assert_fail_containing() {
    local name="$1"
    shift
    local out rc
    set +e
    out="$(run_check 2>&1)"
    rc=$?
    set -e
    if [ "$rc" -eq 0 ]; then
        echo "FAIL [$name]: expected non-zero exit, got 0. Output:"
        echo "$out"
        FAILS=$((FAILS + 1))
        return
    fi
    local needle
    for needle in "$@"; do
        if ! grep -qF -- "$needle" <<<"$out"; then
            echo "FAIL [$name]: exited non-zero (good) but output did not mention expected reason ('$needle'). Output:"
            echo "$out"
            FAILS=$((FAILS + 1))
            return
        fi
    done
    echo "ok   [$name] (exit $rc, mentions: $*)"
}

# --- case 1: each bundled dir / extra file changed, version NOT bumped -> FAIL
for d in ${EXTRA_DIRS[@]+"${EXTRA_DIRS[@]}"}; do
    fresh_repo
    echo "changed" >"${case_dir}/${d}/sample.txt"
    commit_change
    assert_fail_containing "${d}/ changed, no bump" \
        "${d}/sample.txt" "manifest.json" "0.1.0" "FAIL"
done
for f in ${EXTRA_FILES[@]+"${EXTRA_FILES[@]}"}; do
    fresh_repo
    echo "changed" >>"${case_dir}/${f}"
    commit_change
    assert_fail_containing "${f} changed, no bump" "${f}" "manifest.json" "0.1.0" "FAIL"
done

# --- case 2: a shipped file changed, version bumped -> PASS ---------------
fresh_repo
echo "# Fixture plugin, updated" >"${case_dir}/README.md"
bump_version 0.1.1
commit_change
assert_pass "shipped file changed, version bumped"

# --- case 3: README changed, version NOT bumped -> FAIL --------------------
fresh_repo
echo "# Fixture plugin, updated" >"${case_dir}/README.md"
commit_change
assert_fail_containing "README changed, no bump" "README.md" "FAIL"

# --- case 4: only manifest.json touched (version bump itself) -> PASS -----
# The bump is itself a shipped-file change, and the version differs -- must
# not require a SECOND shipped file to also have changed.
fresh_repo
bump_version 0.1.1
commit_change
assert_pass "manifest-only version bump"

# --- case 5: docs-only change -> PASS, no bump required --------------------
fresh_repo
echo "# review" >"${case_dir}/docs/code-reviews/2026-09-10-example.md"
commit_change
assert_pass "docs-only change, no bump required"

# --- case 6: workflow-only change -> PASS, no bump required ----------------
fresh_repo
echo "name: CI (updated)" >"${case_dir}/.github/workflows/ci.yml"
commit_change
assert_pass "workflow-only change, no bump required"

# --- case 6b: a tracked file package.sh does not bundle -> PASS ------------
# Guards against over-inclusion: CLAUDE.md is repo tooling, never shipped.
fresh_repo
echo "# agent rules, updated" >"${case_dir}/CLAUDE.md"
commit_change
assert_pass "non-bundled file changed, no bump required"

# --- case 7: manifest.json changed but NOT the version (e.g. permissions)
# still counts as a shipped-file change, still requires the version to move.
fresh_repo
python3 -c "
import json
m = json.load(open('${case_dir}/manifest.json'))
m['description'] = 'updated blurb'
json.dump(m, open('${case_dir}/manifest.json', 'w'))
"
commit_change
assert_fail_containing "manifest changed without version bump" "manifest.json" "FAIL"

# --- case 8: SHIPPED_PATTERNS mirrors package.sh's own bundle entries -----
# package.sh's `entries=(...)` / `entries+=(...)` lines are the actual source
# of truth for what ships (conditional `[ -f X ] && entries+=(X)` lines
# included -- they are parsed, not hard-coded here). This parses them out of
# the REAL package.sh and asserts, in both directions, against the
# SHIPPED_PATTERNS array of the REAL check-version-bump.sh:
#   * every bundled entry is covered (as "<entry>" or "<entry>/*"), except an
#     entry that is gitignored and untracked (a build artifact such as bin/,
#     which can never appear in `git diff --name-only`); and
#   * no pattern is unexplained: each is a bundled entry, or (compiled
#     plugins only) the build's inputs.
# For a compiled plugin (scripts/build.sh has a `go build` line) the inputs
# that PRODUCE the shipped binary must be covered: build.sh's source dir
# (last argument of `go build`, "./" stripped) as "<dir>/*", go.mod, go.sum
# and scripts/build.sh itself (a dependency bump or a build-flag change
# alters the binary without touching a file under the source dir).
entries_found=$(grep -E '^[^#]*entries\+?=\(' "$PACKAGE_SH" | sed -E 's/.*entries\+?=\(([^)]*)\).*/\1/' | tr ' ' '\n' | grep -v '^$' || true)
if [ -z "$entries_found" ]; then
    echo "FAIL [entries mirror]: could not find package.sh's entries=(...) line"
    FAILS=$((FAILS + 1))
else
    bundle_entries=()
    while IFS= read -r line; do bundle_entries+=("$line"); done <<<"$entries_found"
    # Scoped to the SHIPPED_PATTERNS=(...) array body ONLY, with comments
    # stripped, so a quoted mention in a COMMENT cannot stand in for a real
    # array entry (check-version-bump.sh is deliberately comment-heavy;
    # proved by mutation in tax-de's 2026-09-10 review). `# ...` is stripped
    # before the quoted tokens are read (no pattern contains a '#').
    patterns=$(awk '/^SHIPPED_PATTERNS=\(/ {inside = 1; next}
                    inside && /^\)/ {inside = 0}
                    inside' "$REAL_SCRIPT" | sed 's/#.*//' | grep -oE "'[^']*'" | tr -d "'")

    mismatch=0
    compiled=0
    source_dir=""
    if [ -f "$BUILD_SH" ]; then
        build_line=$(grep -m1 'go build ' "$BUILD_SH" || true)
        if [ -n "$build_line" ]; then
            compiled=1
            # Last whitespace-separated token on the go build line is its
            # source package argument (e.g. "./src"); strip a leading "./".
            source_dir="${build_line##* }"
            source_dir="${source_dir#./}"
        fi
    fi

    allowed="$patterns"
    for entry in "${bundle_entries[@]}"; do
        # A build artifact: gitignored (bare or dir form -- a directory-only
        # pattern like "/bin/" only matches the trailing-slash query when the
        # dir does not exist yet) AND with nothing tracked under it.
        if (cd "$REAL_ROOT" && { git check-ignore -q "$entry" 2>/dev/null || git check-ignore -q "$entry/" 2>/dev/null; }) &&
            [ -z "$(cd "$REAL_ROOT" && git ls-files -- "$entry")" ]; then
            if [ "$compiled" -eq 0 ]; then
                echo "FAIL [entries mirror]: package.sh bundles gitignored '$entry' but build.sh has no 'go build' line to say what produces it"
                mismatch=1
            fi
            continue
        fi
        if ! grep -qxF "$entry" <<<"$patterns" && ! grep -qxF "${entry}/*" <<<"$patterns"; then
            echo "FAIL [entries mirror]: package.sh bundles '$entry' but SHIPPED_PATTERNS in check-version-bump.sh has no '$entry' or '${entry}/*' entry"
            mismatch=1
        fi
    done
    for entry in "${bundle_entries[@]}"; do
        allowed+=$'\n'"$entry"$'\n'"${entry}/*"
    done
    if [ "$compiled" -eq 1 ]; then
        for need in "${source_dir}/*" "go.mod" "go.sum" "scripts/build.sh"; do
            if ! grep -qxF "$need" <<<"$patterns"; then
                echo "FAIL [entries mirror]: the shipped binary is built from '${source_dir}' by scripts/build.sh but SHIPPED_PATTERNS has no '${need}' entry (a change to it rebuilds the binary without touching anything else)"
                mismatch=1
            fi
            allowed+=$'\n'"$need"
        done
    fi
    # scripts/package.sh is the packaging recipe itself, not a bundled entry:
    # always allowed (ut-docs#5147).
    allowed+=$'\n'"scripts/package.sh"
    while IFS= read -r pat; do
        [ -n "$pat" ] || continue
        if ! grep -qxF -- "$pat" <<<"$allowed"; then
            echo "FAIL [entries mirror]: SHIPPED_PATTERNS has '$pat', which package.sh does not bundle and which is not a build input"
            mismatch=1
        fi
    done <<<"$patterns"
    if [ "$mismatch" -eq 0 ]; then
        echo "ok   [entries mirror] (package.sh: ${bundle_entries[*]}; compiled=${compiled}${source_dir:+ from ${source_dir}})"
    else
        FAILS=$((FAILS + 1))
    fi
fi

# --- case 9: base branch ALSO moved forward independently, with its own --
# shipped change + bump (multi-lane / ut-docs#1940-review B1 regression).
# A two-dot diff against a moving base.sha would let that OTHER PR's
# landed bump silently cover for THIS PR's own missing one.
fresh_repo
mergebase_sha="$base_sha"
echo "# main moved" >"${case_dir}/README.md"
bump_version 0.1.1
(cd "$case_dir" && git add -A && git commit -q -m "main advances, bumped")
main_tip_sha=$(cd "$case_dir" && git rev-parse HEAD)
# This PR's own head branches from the ORIGINAL base and changes a shipped
# file without bumping.
(cd "$case_dir" && git checkout -q "$mergebase_sha")
echo "# PR change" >"${case_dir}/README.md"
(cd "$case_dir" && git add -A && git commit -q -m "PR change, no bump")
pr_head_sha=$(cd "$case_dir" && git rev-parse HEAD)
base_sha="$main_tip_sha"
head_sha="$pr_head_sha"
assert_fail_containing "base moved forward independently, PR itself didn't bump" \
    "README.md" "FAIL"

# --- case 10: pre-release version suffix must not crash past the FAIL ----
# path (ut-docs#1940-review B2 regression).
fresh_repo
bump_version 0.1.0-beta.1
(cd "$case_dir" && git add -A && git commit -q -m "adopt pre-release version scheme")
base_sha=$(cd "$case_dir" && git rev-parse HEAD)
echo "# changed" >"${case_dir}/README.md"
commit_change
assert_fail_containing "pre-release version suffix, no bump" "FAIL"

# --- case 11: version differs but is ALREADY TAGGED elsewhere ------------
# (ut-docs#1940-review N1). auto-tag-release.yml no-ops on an existing tag,
# so reusing one is the same silent-never-ships failure with a green guard.
fresh_repo
(cd "$case_dir" && git tag "v0.1.1")
echo "# changed" >"${case_dir}/README.md"
bump_version 0.1.1
commit_change
assert_fail_containing "bumped to an already-tagged version" "FAIL" "v0.1.1" "ALREADY TAGGED"

# --- case 11b: LICENSE changed, version NOT bumped -> FAIL ----------------
fresh_repo
echo "MIT (2026)" >"${case_dir}/LICENSE"
commit_change
assert_fail_containing "LICENSE changed, no bump" "LICENSE" "FAIL"

# --- case 12: src/*.go changed, version NOT bumped -> FAIL ----------------
# The whole point of the compiled-plugin variant: a real behaviour change to
# the WASI module's source, with the built binary never visible to git diff,
# must still be caught.
fresh_repo
printf 'package main\n\nfunc main() {\n\tprintln("changed")\n}\n' >"${case_dir}/src/main.go"
commit_change
assert_fail_containing "src/*.go changed, no bump" "src/main.go" "FAIL"

# --- case 13: src/*.go changed, version bumped -> PASS --------------------
fresh_repo
printf 'package main\n\nfunc main() {\n\tprintln("changed")\n}\n' >"${case_dir}/src/main.go"
bump_version 0.1.1
commit_change
assert_pass "src/*.go changed, version bumped"

# --- case 14: go.mod changed (dependency bump), version NOT bumped -> FAIL
# A dependency bump changes the compiled binary without touching a single
# file under src/ -- must be caught on its own.
fresh_repo
printf 'module github.com/universaltill/fixture\n\ngo 1.23\n\nrequire example.com/newdep v1.0.0\n' >"${case_dir}/go.mod"
commit_change
assert_fail_containing "go.mod changed, no bump" "go.mod" "FAIL"

# --- case 15: nested src/ path added, version NOT bumped -> FAIL ----------
fresh_repo
mkdir -p "${case_dir}/src/sub"
echo 'package sub' >"${case_dir}/src/sub/sub.go"
commit_change
assert_fail_containing "nested src/ file added, no bump" "src/sub/sub.go" "FAIL"

# --- case 17: go.sum changed, version NOT bumped -> FAIL ------------------
# A `go mod tidy` that rewrites go.sum alone still relinks the binary.
fresh_repo
echo "example.com/newdep v1.0.0 h1:abc=" >"${case_dir}/go.sum"
commit_change
assert_fail_containing "go.sum changed, no bump" "go.sum" "FAIL"

# --- case 18: scripts/build.sh changed (build recipe), no bump -> FAIL ----
# The build RECIPE is an input to the binary just as much as src/ is: adding
# -trimpath/-ldflags/-tags, or changing GOOS/GOARCH, produces a different
# shipped binary with src/ byte-identical.
fresh_repo
echo "# rebuilt with -trimpath from here on" >>"${case_dir}/scripts/build.sh"
commit_change
assert_fail_containing "build.sh recipe changed, no bump" "scripts/build.sh" "FAIL"

# --- case 19: a shipped file RENAMED OUT of a shipped location -> FAIL ----
# `git diff --name-only` with rename detection prints only the DESTINATION,
# so `git mv src/x.go docs/x.go` would report a lone `docs/x.go`; the body
# passes --no-renames so the src/ side shows up as a delete.
fresh_repo
mkdir -p "${case_dir}/src/sub"
echo 'package sub' >"${case_dir}/src/sub/sub.go"
(cd "$case_dir" && git add -A && git commit -q -m "add a nested source file")
base_sha=$(cd "$case_dir" && git rev-parse HEAD)
(cd "$case_dir" && git mv src/sub/sub.go docs/sub.go)
commit_change
assert_fail_containing "source file renamed out of src/, no bump" "src/sub/sub.go" "FAIL"

# --- case 20 (ut-docs#4484): first PR of a new repo, no manifest.json at the
# merge-base -> PASS. A new plugin repo starts with a commit that holds no
# manifest (licence, ignore rules, review record); the first PR adds it.
fresh_repo
(cd "$case_dir" && git rm -q manifest.json && git commit -q -m "repo start: no manifest yet")
base_sha=$(cd "$case_dir" && git rev-parse HEAD)
(cd "$case_dir" && git checkout -q HEAD~1 -- manifest.json)
echo "first release" >>"${case_dir}/README.md"
commit_change
assert_pass "first PR: no manifest.json at the merge-base"

# --- case 21 (ut-docs#4484): the first-release path still refuses a version
# that is already tagged -- the same silent never-ships failure.
fresh_repo
first_version=$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["version"])' "${case_dir}/manifest.json")
(cd "$case_dir" && git rm -q manifest.json && git commit -q -m "repo start: no manifest yet" && git tag "v${first_version}")
base_sha=$(cd "$case_dir" && git rev-parse HEAD)
(cd "$case_dir" && git checkout -q HEAD~1 -- manifest.json)
echo "first release" >>"${case_dir}/README.md"
commit_change
assert_fail_containing "first PR: version already tagged" "FAIL" "v${first_version}" "ALREADY TAGGED"

# --- case 22 (ut-docs#4484): manifest.json present at the merge-base but
# unparseable -> FAIL. Only a genuinely ABSENT manifest is a first release.
fresh_repo
echo '{ not json' >"${case_dir}/manifest.json"
(cd "$case_dir" && git add -A && git commit -q -m "broken manifest")
base_sha=$(cd "$case_dir" && git rev-parse HEAD)
(cd "$case_dir" && git checkout -q HEAD~1 -- manifest.json)
echo "change" >>"${case_dir}/README.md"
commit_change
assert_fail_containing "unparseable base manifest still fails closed" "could not read manifest.json version at merge-base"

# --- case 23 (ut-docs#4484): stale branch forked before the first manifest
# -> FAIL. main has since shipped manifest.json (deliberately untagged, so
# only the stale-branch check can reject it); a branch forked from the
# pre-manifest commit must not ride the first-release exemption.
fresh_repo
(cd "$case_dir" && git rm -q manifest.json && git commit -q -m "repo start: no manifest yet")
fork=$(cd "$case_dir" && git rev-parse HEAD)
(cd "$case_dir" && git checkout -q HEAD~1 -- manifest.json && git commit -q -m "first release")
base_sha=$(cd "$case_dir" && git rev-parse HEAD)
(cd "$case_dir" && git checkout -q -b stale "$fork" && git checkout -q "$base_sha" -- manifest.json)
echo "later change" >>"${case_dir}/README.md"
commit_change
assert_fail_containing "stale branch forked before the first manifest" "FAIL" "forked before manifest.json existed"

# --- case 24 (ut-docs#4484): no manifest.json at the merge-base OR the head
# -> PASS. A new repo's early PR before the manifest exists releases nothing.
fresh_repo
(cd "$case_dir" && git rm -q manifest.json && git commit -q -m "repo start: no manifest yet")
base_sha=$(cd "$case_dir" && git rev-parse HEAD)
echo "early docs" >>"${case_dir}/README.md"
commit_change
assert_pass "no manifest.json at the merge-base or the head"

# --- case (ut-docs#4489): a hand-edited shared body fails its self-check --
# scripts/check-version-bump.body.sh must be byte-identical to ut-docs's
# canonical copy; any edit (here: one appended line) fails CI before the
# guard logic runs.
fresh_repo
printf '# hand edit\n' >>"${case_dir}/scripts/check-version-bump.body.sh"
head_sha="$base_sha"
assert_fail_containing "hand-edited body fails its self-check" "differs from the canonical copy"

# --- case (ut-docs#5147): scripts/package.sh changed, no bump -> FAIL -----
# package.sh is the packaging recipe: adding or dropping an entry, or
# changing the tar flags, changes the release artifact without touching any
# bundled file. The fixture has no package.sh, so one is committed first.
fresh_repo
printf '#!/usr/bin/env bash\nentries=(manifest.json README.md)\n' >"${case_dir}/scripts/package.sh"
(cd "$case_dir" && git add -A && git commit -q -m "add package.sh")
base_sha=$(cd "$case_dir" && git rev-parse HEAD)
echo 'entries+=(docs)' >>"${case_dir}/scripts/package.sh"
commit_change
assert_fail_containing "package.sh recipe changed, no bump" "scripts/package.sh" "FAIL"

# --- case (ut-docs#5147): scripts/package.sh changed, version bumped -> PASS
fresh_repo
printf '#!/usr/bin/env bash\nentries=(manifest.json README.md)\n' >"${case_dir}/scripts/package.sh"
(cd "$case_dir" && git add -A && git commit -q -m "add package.sh")
base_sha=$(cd "$case_dir" && git rev-parse HEAD)
echo 'entries+=(docs)' >>"${case_dir}/scripts/package.sh"
python3 -c "
import json
m = json.load(open('${case_dir}/manifest.json'))
m['version'] = '9.9.9'
json.dump(m, open('${case_dir}/manifest.json', 'w'))
"
commit_change
assert_pass "package.sh recipe changed, version bumped"

if [ "$FAILS" -ne 0 ]; then
    echo ""
    echo "$FAILS case(s) failed."
    exit 1
fi
echo ""
echo "All check-version-bump.sh cases passed."
