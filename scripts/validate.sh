#!/usr/bin/env bash
# Validates the payment-plugin manifest: marketplace-required fields
# (id/name/semver/permissions/locales), asset-only runtime, and exactly one
# type="payment" entry with a trigger_event.
set -euo pipefail
cd "$(dirname "$0")/.."
python3 - <<'PY'
import json, os, re, sys
m = json.load(open("manifest.json"))
errs = []
if not re.match(r'^[a-z0-9]+([.-][a-z0-9]+)*$', m.get("id","")): errs.append("bad id")
if not m.get("name"): errs.append("missing name")
if not re.match(r'^\d+\.\d+\.\d+', m.get("version","")): errs.append("bad version")
if not m.get("permissions"): errs.append("missing permissions")
if not m.get("locales"): errs.append("missing locales")
if m.get("runtime") not in ("none", "wasm"): errs.append("runtime must be 'none' or 'wasm' (ADR-0001)")
if m.get("runtime") == "wasm":
    ep = (m.get("entrypoint") or "").lstrip("./")
    if not ep.endswith(".wasm"): errs.append("wasm runtime needs a .wasm entrypoint")
    elif not os.path.isfile(ep): errs.append(f"module not found: {ep} (run scripts/build.sh)")
if m.get("device_arch") != "any": errs.append("device_arch must be 'any'")
if m.get("canonical_type") != "payment": errs.append("canonical_type must be 'payment'")
pays = [e for e in m.get("entries", []) if e.get("type") == "payment"]
if len(pays) != 1:
    errs.append(f"expected exactly 1 payment entry, got {len(pays)}")
else:
    if not pays[0].get("key"): errs.append("payment entry missing key")
    if not pays[0].get("label"): errs.append("payment entry missing label")
    if not pays[0].get("trigger_event"): errs.append("payment entry missing trigger_event")
if errs:
    print("FAIL: " + "; ".join(errs)); sys.exit(1)
print(f"ok {m['id']} v{m['version']}")
PY
