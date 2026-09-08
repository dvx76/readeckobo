#!/bin/sh

if [ -z "$1" ]; then
  echo "Usage: $0 <USER_TOKEN>"
  echo "  POSTs a single annotation item with an UNKNOWN bookmark_id and asserts"
  echo "  the per-item JSON result reports status \"error\" gracefully (HTTP 200,"
  echo "  results[0].error populated) instead of failing the whole batch."
  exit 1
fi

USER_TOKEN="$1"
BASE_URL="http://localhost:8080"
TMP_FILE="$(mktemp)"

echo "Testing: POST /api/agent/annotations (unknown bookmark_id)"

# A minimal but well-formed item: bookmark_id is intentionally bogus so the
# server must fail just that item with status=error.
PAYLOAD='{"device":"e2e-test-device","items":[{"bookmark_id":"does-not-exist-00000000","bookmark_row_id":"00000000-0000-0000-0000-000000000000","start_path":"OEBPS/xhtml/ch001.xhtml#kobo.1.1","start_offset":0,"end_path":"OEBPS/xhtml/ch001.xhtml#kobo.1.1","end_offset":5,"text":"test","type":"highlight","date_created":"2026-01-01T00:00:00Z","date_modified":"2026-01-01T00:00:00Z"}]}'

curl -s --fail-with-body -o "$TMP_FILE" -w 'http status: %{http_code}\n' \
  -X POST \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $USER_TOKEN" \
  -d "$PAYLOAD" \
  "$BASE_URL/api/agent/annotations"
CURL_STATUS=$?

if [ "$CURL_STATUS" -ne 0 ]; then
  echo "ERROR: curl exited with $CURL_STATUS"
  echo "---- response body ----"
  cat "$TMP_FILE"
  echo "-----------------------"
  rm -f "$TMP_FILE"
  exit 1
fi

echo "---- response body ----"
cat "$TMP_FILE"
echo "-----------------------"

# The server responds 200 with results[] even when an item fails; assert the
# per-item shape: exactly one result for the sent row, with an error status.
if ! grep -q '"bookmark_row_id"' "$TMP_FILE"; then
  echo "ERROR: response has no results[] entries (missing per-item shape)"
  rm -f "$TMP_FILE"
  exit 1
fi

if ! grep -q '"status": *"error"' "$TMP_FILE"; then
  echo "ERROR: per-item status is not \"error\" for the unknown bookmark"
  rm -f "$TMP_FILE"
  exit 1
fi

if ! grep -q '"error"' "$TMP_FILE"; then
  echo "ERROR: per-item result has no error message"
  rm -f "$TMP_FILE"
  exit 1
fi

rm -f "$TMP_FILE"
echo "PASS"
