#!/usr/bin/env bash
# Approves (review-assign + approve, which Ed25519-signs) the release recorded
# in dist/publish-response.json. Intended for the DEV marketplace where the
# pipeline is trusted end-to-end; production keeps a human review.
#
# Environment:
#   MARKETPLACE_BASE_URL, MARKETPLACE_UPLOAD_TOKEN (admin gate), REVIEWER (opt)
set -euo pipefail
cd "$(dirname "$0")/.."

: "${MARKETPLACE_BASE_URL:?MARKETPLACE_BASE_URL is required}"
REVIEWER=${REVIEWER:-release-pipeline}
RELEASE_ID=$(python3 -c "import json;print(json.load(open('dist/publish-response.json'))['data']['release_id'])")
BASE="${MARKETPLACE_BASE_URL%/}/ui/api/admin/releases/${RELEASE_ID}"
AUTH=()
[ -n "${MARKETPLACE_UPLOAD_TOKEN:-}" ] && AUTH=(--header "Authorization: Bearer ${MARKETPLACE_UPLOAD_TOKEN}")

echo "==> Assigning review for ${RELEASE_ID}"
curl --silent --show-error --fail "${AUTH[@]}" -X POST "${BASE}/assign-review" \
  -d "reviewer_id=${REVIEWER}&priority=P1" && echo ""

echo "==> Approving ${RELEASE_ID}"
curl --silent --show-error --fail "${AUTH[@]}" -X POST "${BASE}/review-decision" \
  -d "decision=approved&reviewed_by=${REVIEWER}&comments=auto-approved by release pipeline (dev marketplace)" && echo ""
