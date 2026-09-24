#!/usr/bin/env bash
# vulncheck.sh - govulncheck with an explicit, documented allowlist.
#
# govulncheck has no ignore mechanism; CI must still fail on any new finding
# while not failing forever on an advisory that has no fix and no reachable
# code path. Each allowed ID lives in .govulncheck-allow with the reason and
# the date it was reviewed; anything else that govulncheck reports as
# affecting this code fails the check.
set -euo pipefail
allow=".govulncheck-allow"
out="$(mktemp)"
trap 'rm -f "$out"' EXIT
govulncheck -format json ./... >"$out" || true
mapfile -t found < <(jq -r 'select(.finding != null) | select(.finding.trace[0].function != null) | .finding.osv' "$out" | sort -u)
status=0
for id in "${found[@]}"; do
  if grep -qE "^$id\b" "$allow" 2>/dev/null; then
    echo "vulncheck: $id is allowlisted: $(grep -E "^$id\b" "$allow" | cut -d' ' -f2-)"
  else
    echo "vulncheck: $id affects this code and is not allowlisted" >&2
    jq -r --arg id "$id" 'select(.osv != null) | select(.osv.id == $id) | "  \(.osv.summary)"' "$out" >&2
    status=1
  fi
done
[ "$status" -eq 0 ] && echo "vulncheck: ok (${#found[@]} findings, all allowlisted)"
exit "$status"
