#!/usr/bin/env bash
# Packages the plugin into the canonical marketplace release artifact:
#   dist/<plugin-id>_<version>_universal.tar.gz
# Asset-only plugin (runtime "none") => one universal artifact for every
# os/arch. manifest.json sits at the archive root; no "./" members (the POS
# importer rejects them as path traversal).
set -euo pipefail
cd "$(dirname "$0")/.."

scripts/validate.sh

ID=$(python3 -c "import json;print(json.load(open('manifest.json'))['id'])")
VERSION=$(python3 -c "import json;print(json.load(open('manifest.json'))['version'])")
OUT="dist/${ID}_${VERSION}_universal.tar.gz"
mkdir -p dist

entries=(manifest.json README.md bin)
[ -f LICENSE ] && entries+=(LICENSE)
# COPYFILE_DISABLE stops macOS tar shipping AppleDouble ._* junk (the
# marketplace bundle-hygiene gate rejects it).
COPYFILE_DISABLE=1 tar -czf "$OUT" "${entries[@]}"
if command -v sha256sum >/dev/null 2>&1; then sha256sum "$OUT" > "${OUT}.sha256"; else shasum -a 256 "$OUT" > "${OUT}.sha256"; fi
echo "packaged $OUT"
