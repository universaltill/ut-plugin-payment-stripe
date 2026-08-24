#!/usr/bin/env bash
# Approves (review-assign + approve, which Ed25519-signs) the release recorded
# in dist/publish-response.json. Intended for the DEV marketplace where the
# pipeline is trusted end-to-end; production keeps a human review.
#
# Environment:
#   MARKETPLACE_BASE_URL, MARKETPLACE_ADMIN_TOKEN (admin gate; ut-cloud's
#   authorizeStaff has honored a distinct admin token since ut-docs#496 —
#   set this as its own repo secret, separate from MARKETPLACE_UPLOAD_TOKEN,
#   to grant staff-only admin actions without also handing out vendor-upload
#   access), MARKETPLACE_UPLOAD_TOKEN (vendor-upload credential; still the
#   fallback below when MARKETPLACE_ADMIN_TOKEN is unset), REVIEWER (opt)
set -euo pipefail
cd "$(dirname "$0")/.."

: "${MARKETPLACE_BASE_URL:?MARKETPLACE_BASE_URL is required}"
REVIEWER=${REVIEWER:-release-pipeline}
RELEASE_ID=$(python3 -c "import json;print(json.load(open('dist/publish-response.json'))['data']['release_id'])")
BASE="${MARKETPLACE_BASE_URL%/}/ui/api/admin/releases/${RELEASE_ID}"
# Admin/vendor token conflation (ut-docs#166): MARKETPLACE_UPLOAD_TOKEN is a
# vendor-upload credential, not an admin credential, and prefers a distinct
# MARKETPLACE_ADMIN_TOKEN when one is set. As of ut-docs#496,
# MARKETPLACE_ADMIN_TOKEN does something real server-side — once configured,
# ut-cloud's authorizeStaff uses it EXCLUSIVELY for staff-only admin routes
# (no fallback to the upload token server-side), so setting it as a distinct
# repo secret genuinely narrows this pipeline's credential to staff-only
# admin actions instead of the broader vendor-upload one. Leaving it unset
# keeps the pre-#496 behavior: this script falls back to the upload token
# client-side, no change required.
ADMIN_TOKEN="${MARKETPLACE_ADMIN_TOKEN:-}"
if [ -z "$ADMIN_TOKEN" ] && [ -n "${MARKETPLACE_UPLOAD_TOKEN:-}" ]; then
  ADMIN_TOKEN="${MARKETPLACE_UPLOAD_TOKEN}"
fi
AUTH=()
[ -n "$ADMIN_TOKEN" ] && AUTH=(--header "Authorization: Bearer ${ADMIN_TOKEN}")

echo "==> Assigning review for ${RELEASE_ID}"
curl --silent --show-error --fail "${AUTH[@]}" -X POST "${BASE}/assign-review" \
  -d "reviewer_id=${REVIEWER}&priority=P1" && echo ""

echo "==> Approving ${RELEASE_ID}"
curl --silent --show-error --fail "${AUTH[@]}" -X POST "${BASE}/review-decision" \
  -d "decision=approved&reviewed_by=${REVIEWER}&comments=auto-approved by release pipeline (dev marketplace)" && echo ""
