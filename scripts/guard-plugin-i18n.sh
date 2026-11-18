#!/usr/bin/env bash
# Reusable plugin-repo i18n drift guard template (ut-docs#1882).
#
# Any ut-plugin-* repo can copy this file verbatim into its own scripts/
# and wire it into CI (see architecture/plugin-architecture.md §7). It
# checks a plugin's own locales/*.json overlay files — the convention
# ADR-0010 / plugin-architecture.md §7 documents — for the same drift class
# universal-till's own scripts/ci/guard-i18n.sh catches in core:
#
#   1. no non-base locale file has an ORPHAN key (one locales/en.json lacks).
#      A key MISSING from a non-base locale is only a printed note, not a
#      failure: code PRs add English only, other languages come later
#      (Weblate), and the till falls back to en.json (ADR-0160 §2,
#      ut-docs#6570).
#   2. every locale value is a flat string — a nested JSON object would
#      silently defeat check 1's key-set comparison (it only ever looks at
#      TOP-level keys, so drift inside a nested object is invisible to it)
#      and can't round-trip through config.I18n.T() anyway, which expects a
#      plain string. plugin-architecture.md §7's own convention is a flat,
#      prefixed key (`tax_de.rate_label`), not a nested object — this
#      check makes that the only shape the guard will accept, rather than
#      silently no-op'ing on anything else (found in review, ut-docs#1882).
#   3. every locale value's printf/template verbs (%d, %s, {{name}}, {0},
#      ...) match en.json's for that key — ported verbatim from
#      guard-i18n.sh check #8 (ut-docs#1865/#1873): checks 1-2 above only
#      compare key SETS, so a translation could drop/invent/change a verb
#      while keeping the same key and ship a live `%!d(MISSING)` (found in
#      review, ut-docs#1882 — the same defect class core already hit once).
#   4. no locale file defines the same top-level key twice — json.load
#      silently keeps only the last one, so this re-scans the raw text
#      instead of trusting the parsed object (mirrors guard-i18n.sh check
#      #9, ut-docs#1872).
#   5. every manifest.json entries[].label that is KEY-SHAPED (a flat,
#      lowercase, dot-namespaced token like `tax_de.rate_label` — never a
#      literal with spaces/punctuation a human would actually read) must
#      resolve in locales/en.json (ut-docs#1883 review, F1/F2). This is
#      the one check that runs even with NO locales/ directory at all: a
#      key-shaped label with nothing backing it is exactly the class of
#      bug that shipped a real German-pilot regression — ut-plugin-tax-de
#      0.5.4 changed two entries[].label values to locale keys, but its
#      OWN scripts/package.sh didn't include locales/ in the release
#      artifact, so `syncLocales()` found nothing to overlay and the raw
#      key (e.g. `tax_de.entry_dsfinvk_export_label`) would have rendered
#      to a merchant instead of "DSFinV-K Export (fiskaly)" — this guard
#      existed at the time and would NOT have caught it, because it only
#      checks locale-file-to-locale-file drift, never manifest-to-locale
#      resolution. This check closes that specific gap; it does not (and
#      cannot) verify the PACKAGED artifact actually contains locales/ —
#      that's `scripts/package.sh`'s own job, verify its `entries=(...)`
#      array separately.
#
# Deliberately narrower than guard-i18n.sh's full check list otherwise: a
# plugin ships a WASM/asset bundle, not a Go html/template app, so no
# plugin repo in this org ships a `{{ T "key" }}` template, a Go-side
# w.Write/RenderError call site, or a ToastMessage field for checks
# 1/3/6/7 of the core guard to scan. As of 2026-09-10 (ut-docs#1883), a
# plugin's `page`-type entries (pre-existing, plugin_page.go) and
# `export`/`report`-type entries (this card, settings.html) resolve their
# manifest label through core's `T` via this locales/*.json overlay
# mechanism; `payment`/`theme`/`button`-type entry labels do NOT — those
# still render whatever literal string the manifest carries, with no
# translation path at all (a separate, known, cross-cutting core gap, tracked outside this
# card — see architecture/plugin-architecture.md §7's own note on this).
# Settings-field label/help/option keys DO resolve through T since
# ut-docs#2832.
# If a plugin's own UI surface grows a template/render path later, extend
# this template rather than assuming key-set parity alone still covers it.
#
# A plugin with no locales/ directory at all passes cleanly — shipping
# translations is optional (ADR-0010) — so a plugin can wire this guard in
# on day one, before its first locale file ever exists, and get drift
# protection from the very first key it adds rather than only once someone
# remembers to add the guard later.
#
# Usage: scripts/guard-plugin-i18n.sh [ROOT_DIR]
#   ROOT_DIR defaults to the current directory; a plugin repo's own CI step
#   normally calls this with no argument, run from the repo root.
set -euo pipefail

ROOT_DIR="${1:-$(pwd)}"
cd "$ROOT_DIR"

BASE="locales/en.json"

# Check 5 (see header): manifest.json's entries[].label, if key-shaped, must
# resolve in locales/en.json — runs unconditionally, even with no locales/
# directory at all, because that absence is exactly the failure this check
# exists to catch.
if [ -f manifest.json ]; then
  python3 - "$BASE" <<'PY'
import json, re, sys

base_path = sys.argv[1]
try:
    manifest = json.load(open("manifest.json", encoding="utf-8"))
except (OSError, json.JSONDecodeError) as e:
    print(f"guard-plugin-i18n: manifest.json: cannot read/parse: {e}", file=sys.stderr)
    sys.exit(1)

# A key looks like `tax_de.rate_label` or `payment_sumup.decline_reason`:
# flat, lowercase, underscore/digit segments joined by literal dots — never
# a literal a human wrote (spaces, parens, uppercase, punctuation).
KEY_RE = re.compile(r'^[a-z][a-z0-9_]*(\.[a-z0-9_]+)+$')

key_labels = []  # (entry key, label)
for entry in manifest.get("entries", []) or []:
    label = entry.get("label", "")
    if isinstance(label, str) and KEY_RE.match(label):
        key_labels.append((entry.get("key", "?"), label))

if not key_labels:
    sys.exit(0)  # nothing key-shaped to check; fall through to bash below

import os
if not os.path.isfile(base_path):
    print(f"guard-plugin-i18n: manifest.json has {len(key_labels)} key-shaped "
          f"entries[].label value(s) but {base_path} does not exist:")
    for entry_key, label in key_labels:
        print(f"  entries[key={entry_key!r}].label = {label!r}")
    print("  a key-shaped label with no locales/en.json to resolve it renders "
          "as this literal key to every merchant, in every locale — either "
          "add locales/en.json (and confirm scripts/package.sh actually ships "
          "locales/ in the release artifact), or use a plain human-readable "
          "literal for this label instead of a key.", file=sys.stderr)
    sys.exit(1)

base = json.load(open(base_path, encoding="utf-8"))
missing = [(k, lbl) for k, lbl in key_labels if lbl not in base]
if missing:
    print(f"guard-plugin-i18n: manifest.json entries[].label key(s) not found in {base_path}:")
    for entry_key, label in missing:
        print(f"  entries[key={entry_key!r}].label = {label!r} — no such key in {base_path}")
    sys.exit(1)
print(f"guard-plugin-i18n: {len(key_labels)} manifest entries[].label key(s) resolve in {base_path}")
PY
fi

if [ ! -d locales ]; then
  echo "guard-plugin-i18n: no locales/ directory — nothing further to check"
  exit 0
fi

if [ ! -f "$BASE" ]; then
  echo "guard-plugin-i18n: locales/ exists but $BASE is missing — every plugin locale overlay needs an en.json base" >&2
  exit 1
fi

python3 - "$BASE" <<'PY'
import glob, json, re, sys

base_path = sys.argv[1]
fail = False
locale_files = sorted(glob.glob("locales/*.json"))

def load_raw(path):
    # Friendly errors for malformed input instead of a raw traceback
    # (found in review, ut-docs#1882): empty file, invalid JSON, or a
    # UTF-8 BOM (plausible from a Windows-authored translation) all land
    # here rather than crashing check 1/2/3 below.
    try:
        with open(path, encoding="utf-8-sig") as f:
            return f.read()
    except OSError as e:
        print(f"guard-plugin-i18n: {path}: cannot read: {e}")
        sys.exit(1)

def duplicate_keys(path, text):
    # Reads the raw pair list via object_pairs_hook instead of the parsed
    # dict, so a key defined twice anywhere in the file is visible even
    # though json.load would otherwise keep only the last write and hide
    # it from every check below (mirrors universal-till/scripts/ci/
    # guard-i18n.sh check #9, ut-docs#1872). Fires per JSON object, so a
    # duplicate inside a nested object is caught too, not just at the top
    # level, even though check 2 below then separately rejects nesting.
    dupes = []

    def hook(pairs):
        seen = set()
        for k, _ in pairs:
            if k in seen:
                dupes.append(k)
            seen.add(k)
        return dict(pairs)

    json.loads(text, object_pairs_hook=hook)
    return sorted(set(dupes))

def load_locale(path):
    text = load_raw(path)
    try:
        data = json.loads(text)
    except json.JSONDecodeError as e:
        print(f"guard-plugin-i18n: {path}: invalid JSON: {e}")
        sys.exit(1)
    if not isinstance(data, dict):
        print(f"guard-plugin-i18n: {path}: root must be a JSON object (flat key -> string map), got {type(data).__name__}")
        sys.exit(1)
    non_strings = sorted(k for k, v in data.items() if not isinstance(v, str))
    if non_strings:
        print(f"guard-plugin-i18n: {path}: value(s) for {non_strings} are not plain strings — "
              f"nested objects/arrays/numbers aren't supported here; use a flat, "
              f"plugin-id-prefixed key instead (plugin-architecture.md §7, e.g. \"tax_de.rate_label\")")
        sys.exit(1)
    for k in duplicate_keys(path, text):
        print(f"guard-plugin-i18n: {path} defines key {k!r} more than once")
        sys.exit(1)
    return data

# Verb/template-token parity (ported from universal-till/scripts/ci/
# guard-i18n.sh check #8, ut-docs#1865/#1873 — see that script's own header
# comment for the full design rationale behind this regex and the
# positional-vs-implicit-verb rules below).
verb_re = re.compile(
    r'%%'
    r'|%\[(\d+)\]\d*(?:\.\d+)?[vTtbcdoOqxXUeEfFgGspw](?![A-Za-z])'
    r'|%\d*(?:\.\d+)?[vTtbcdoOqxXUeEfFgGspw](?![A-Za-z])'
    r'|\{\{[^{}]*\}\}'
    r'|\{\d+\}'
)

def verb_tokens(s):
    out = []
    for m in verb_re.finditer(s):
        tok = m.group(0)
        if tok == '%%':
            continue
        if tok.startswith('{'):
            out.append((tok, None, True))
        else:
            out.append((tok, int(m.group(1)) if m.group(1) else None, False))
    return out

def verb_shape(tok):
    return re.sub(r'^%\[\d+\]', '%', tok)

def printf_tokens(toks):
    return [(t, idx) for t, idx, is_tmpl in toks if not is_tmpl]

def template_tokens(toks):
    return [t for t, _, is_tmpl in toks if is_tmpl]

def mixes_positional_and_implicit(a_pf, b_pf):
    def fully_positional(toks):
        return bool(toks) and all(idx is not None for _, idx in toks)
    a_pos = any(idx is not None for _, idx in a_pf)
    b_pos = any(idx is not None for _, idx in b_pf)
    return (a_pos or b_pos) and not (fully_positional(a_pf) and fully_positional(b_pf))

def percent_literal_count(s):
    return sum(1 for m in verb_re.finditer(s) if m.group(0) == '%%')

def verbs_match(a, b):
    a_toks, b_toks = verb_tokens(a), verb_tokens(b)
    if percent_literal_count(a) != percent_literal_count(b):
        return False
    if template_tokens(a_toks) != template_tokens(b_toks):
        return False
    a_pf, b_pf = printf_tokens(a_toks), printf_tokens(b_toks)
    a_pos = any(idx is not None for _, idx in a_pf)
    b_pos = any(idx is not None for _, idx in b_pf)
    if not a_pos and not b_pos:
        return [t for t, _ in a_pf] == [t for t, _ in b_pf]
    if mixes_positional_and_implicit(a_pf, b_pf):
        return False
    a_map = {idx: verb_shape(t) for t, idx in a_pf}
    b_map = {idx: verb_shape(t) for t, idx in b_pf}
    return a_map == b_map

locales = {path: load_locale(path) for path in locale_files}

base = set(locales[base_path].keys())
for path in locale_files:
    if path == base_path:
        continue
    ks = set(locales[path].keys())
    only_base = sorted(base - ks)
    only_loc = sorted(ks - base)
    if only_base:
        # Not a failure (ADR-0160 §2): the till falls back to en.json.
        print(f"guard-plugin-i18n: note: {path} lacks {len(only_base)} key(s) from {base_path} (falls back to English):")
        for k in only_base:
            print("  missing:", k)
    if only_loc:
        fail = True
        print(f"guard-plugin-i18n: {path} has keys not in {base_path}:")
        for k in only_loc:
            print("  orphan :", k)

base_values = locales[base_path]
for path in locale_files:
    if path == base_path:
        continue
    loc_values = locales[path]
    for k in sorted(base_values.keys() & loc_values.keys()):
        if not verbs_match(base_values[k], loc_values[k]):
            fail = True
            print(f"guard-plugin-i18n: {path}: {k}: format/template verb mismatch against {base_path} "
                  f"(dropped, invented, changed, or un-declared reordering): "
                  f"en={[t for t,_,_ in verb_tokens(base_values[k])]} vs "
                  f"this={[t for t,_,_ in verb_tokens(loc_values[k])]}")

if fail:
    sys.exit(1)
print(f"guard-plugin-i18n: ok ({len(base)} key(s) across {len(locale_files)} locale file(s))")
PY
