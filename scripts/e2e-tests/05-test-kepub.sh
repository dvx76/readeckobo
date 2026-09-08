#!/bin/sh

if [ -z "$1" ] || [ -z "$2" ]; then
  echo "Usage: $0 <USER_TOKEN> <BOOKMARK_ID>"
  echo "  Fetches GET /api/kepub/<BOOKMARK_ID> and asserts it is a zip"
  echo "  (application/epub+zip) rather than an error body."
  exit 1
fi

USER_TOKEN="$1"
BOOKMARK_ID="$2"
BASE_URL="http://localhost:8080"
TMP_FILE="$(mktemp)"

echo "Testing: GET /api/kepub/$BOOKMARK_ID"

# Save the body so we can inspect the content-type and size, and probe the
# bytes with file/unzip. --fail-with-body surfaces non-2xx responses as errors.
CONTENT_TYPE=$(curl -s --fail-with-body -o "$TMP_FILE" -w '%{content_type}' \
  "$BASE_URL/api/kepub/$BOOKMARK_ID?token=$USER_TOKEN")
CURL_STATUS=$?

if [ "$CURL_STATUS" -ne 0 ]; then
  echo "ERROR: curl exited with $CURL_STATUS"
  echo "---- response body ----"
  cat "$TMP_FILE"
  echo "-----------------------"
  rm -f "$TMP_FILE"
  exit 1
fi

SIZE=$(wc -c < "$TMP_FILE")
echo "content-type: $CONTENT_TYPE"
echo "size:         $SIZE bytes"

if [ "$SIZE" -eq 0 ]; then
  echo "ERROR: empty response body"
  rm -f "$TMP_FILE"
  exit 1
fi

# Assert the payload is a real zip (PK\x03\x04 magic) / valid archive.
MAGIC=$(od -An -tx1 -N4 "$TMP_FILE" | tr -d ' \n')
if [ "$MAGIC" = "504b0304" ]; then
  echo "magic bytes:  PK (zip) — OK"
else
  echo "magic bytes:  $MAGIC — ERROR: not a zip"
  echo "---- response body (first 2KB) ----"
  head -c 2048 "$TMP_FILE"
  echo ""
  echo "------------------------------------"
  rm -f "$TMP_FILE"
  exit 1
fi

if command -v unzip >/dev/null 2>&1; then
  if unzip -tq "$TMP_FILE" >/dev/null 2>&1; then
    echo "unzip -t:     archive valid — OK"
  else
    echo "ERROR: unzip -t failed (corrupt archive)"
    rm -f "$TMP_FILE"
    exit 1
  fi
  echo "---- contents ----"
  unzip -l "$TMP_FILE" | tail -n +2 | head -n 20
  echo "------------------"
fi

rm -f "$TMP_FILE"
echo "PASS"
