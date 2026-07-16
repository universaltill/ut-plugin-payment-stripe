#!/usr/bin/env bash
# Publishes the packaged plugin to the marketplace vendor upload API and saves
# the response (incl. release_id) to dist/publish-response.json for the
# approve step.
#
# Environment:
#   MARKETPLACE_BASE_URL      e.g. https://marketplace.home.taskrunnertech.co.uk
#   MARKETPLACE_UPLOAD_TOKEN  bearer token when the marketplace enforces one
#   MARKETPLACE_LISTING_ID    optional: attach the release to a known listing
#   MARKETPLACE_CHANNEL       default: stable
set -euo pipefail
cd "$(dirname "$0")/.."

: "${MARKETPLACE_BASE_URL:?MARKETPLACE_BASE_URL is required}"
CHANNEL=${MARKETPLACE_CHANNEL:-stable}
UPLOAD_URL="${MARKETPLACE_BASE_URL%/}/ui/api/vendor/releases/upload"

ID=$(python3 -c "import json;print(json.load(open('manifest.json'))['id'])")
VERSION=$(python3 -c "import json;print(json.load(open('manifest.json'))['version'])")
ARTIFACT="dist/${ID}_${VERSION}_universal.tar.gz"
[ -f "$ARTIFACT" ] || { echo "ERROR: $ARTIFACT missing (run scripts/package.sh)"; exit 1; }
CHECKSUM=$(cut -d' ' -f1 "${ARTIFACT}.sha256")

args=(
  --silent --show-error --write-out '\n%{http_code}'
  --form "artifact=@${ARTIFACT};type=application/gzip"
  --form "manifest=<manifest.json"
  --form "plugin_id=${ID}"
  --form "version=${VERSION}"
  --form "channel=${CHANNEL}"
  --form "expected_hash=${CHECKSUM}"
  --form "release_notes=Stripe payment release ${VERSION}"
)
[ -n "${MARKETPLACE_LISTING_ID:-}" ] && args+=(--form "listing_id=${MARKETPLACE_LISTING_ID}")
[ -n "${MARKETPLACE_UPLOAD_TOKEN:-}" ] && args+=(--header "Authorization: Bearer ${MARKETPLACE_UPLOAD_TOKEN}")

echo "==> Publishing ${ID} ${VERSION} (${CHANNEL}) to ${UPLOAD_URL}"
RESPONSE=$(curl "${args[@]}" "$UPLOAD_URL")
CODE=${RESPONSE##*$'\n'}
BODY=${RESPONSE%$'\n'*}
echo "    HTTP ${CODE}"
echo "$BODY"
[[ "$CODE" == 2* ]] || { echo "ERROR: upload failed"; exit 1; }
printf '%s' "$BODY" > dist/publish-response.json
