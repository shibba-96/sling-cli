#!/usr/bin/env bash
# Self-contained streaming-queue fail-fast test (two independent groups).
#
# Group 1: search  -> details_a, details_b   (queue.item_ids)
# Group 2: search2 -> details2_a, details2_b  (queue.item_ids2)
#
#   1. Success path: all 6 streams succeed. Assert each child produced exactly
#      NUM_ITEMS records (broadcast + count match).
#   2. Fail-fast path: the server fails one item for group 1 only (grp=1). Assert
#      the run exits non-zero AND fast, that group 1 was terminated (incomplete),
#      and that the independent group 2 still finished successfully.
#
# Prints "QUEUE STREAMING TEST PASSED" on success (asserted by the suite).

set -uo pipefail
cd "$(dirname "$0")"

NUM_ITEMS=30
OUT_DIR="$(mktemp -d "${TMPDIR:-/tmp}/queue_stream_test.XXXXXX")"
PORT_FILE="$OUT_DIR/port"
PORT=""
SERVER_PID=""

cleanup() {
  [[ -n "$SERVER_PID" ]] && kill "$SERVER_PID" >/dev/null 2>&1
  rm -rf "$OUT_DIR"
}
trap cleanup EXIT

fail() { echo "TEST FAILURE: $*" >&2; exit 1; }

# locate the sling binary (built by the suite into cmd/sling)
SLING_BIN="$(cd ../../../cmd/sling && pwd)/sling"
[[ -x "$SLING_BIN" ]] || SLING_BIN="sling"

run_sling() {
  "$SLING_BIN" "$@"
}

count_records() {
  # count JSON objects (one record per {...}) in the child output file
  local f="$1"
  [[ -f "$f" ]] || { echo 0; return; }
  grep -o '"id"' "$f" | wc -l | tr -d ' '
}

# start_server [FAIL_ON_ID] [FAIL_ON_GROUP]
start_server() {
  rm -f "$PORT_FILE"
  NUM_ITEMS="$NUM_ITEMS" PORT=0 PORT_FILE="$PORT_FILE" \
    FAIL_ON_ID="${1:-}" FAIL_ON_GROUP="${2:-}" \
    go run server.go &
  SERVER_PID=$!
  # wait for the server to publish its chosen port, then probe readiness
  for _ in $(seq 1 100); do
    if [[ -s "$PORT_FILE" ]]; then
      PORT="$(cat "$PORT_FILE")"
      if curl -sf "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then
        export TEST_BASE_URL="http://127.0.0.1:$PORT"
        return 0
      fi
    fi
    sleep 0.2
  done
  fail "test server did not become ready"
}

stop_server() {
  if [[ -n "$SERVER_PID" ]]; then
    kill "$SERVER_PID" >/dev/null 2>&1
    wait "$SERVER_PID" 2>/dev/null
    SERVER_PID=""
  fi
}

export OUT_DIR

# ---------------------------------------------------------------------------
echo "=== [1/2] success path: 2 groups, 4 children, expect $NUM_ITEMS each ==="
start_server "" ""

if ! run_sling run -d -r replication.success.yaml; then
  fail "success-path replication failed unexpectedly (exit $?)"
fi
stop_server

for child in details_a details_b details2_a details2_b; do
  n=$(count_records "$OUT_DIR/$child.json")
  echo "  $child => $n records"
  [[ "$n" == "$NUM_ITEMS" ]] || fail "$child produced $n records, expected $NUM_ITEMS (broadcast/count mismatch)"
done
echo "SUCCESS PATH OK: all 4 children produced $NUM_ITEMS records each"

# ---------------------------------------------------------------------------
echo "=== [2/2] fail-fast path: fail group 1 only; group 2 must survive ==="
rm -rf "$OUT_DIR"/*.json
start_server "item-005" "1"

LOG="$OUT_DIR/failfast.log"
start_ts=$(date +%s)
run_sling run -d -r replication.failfast.yaml >"$LOG" 2>&1
rc=$?
end_ts=$(date +%s)
stop_server

cat "$LOG"

# the run as a whole must fail (group 1 failed)
[[ "$rc" == "0" ]] && fail "fail-fast replication unexpectedly succeeded (group 1 should have failed the run)"

# must fail fast (not hang waiting on a terminated group)
elapsed=$(( end_ts - start_ts ))
echo "  fail-fast run exited non-zero in ${elapsed}s"
[[ "$elapsed" -le 60 ]] || fail "fail-fast run took ${elapsed}s — expected to fail fast"

# the independent group 2 must have finished successfully with full output
for child in details2_a details2_b; do
  n=$(count_records "$OUT_DIR/$child.json")
  echo "  group2 $child => $n records"
  [[ "$n" == "$NUM_ITEMS" ]] || fail "independent $child produced $n records, expected $NUM_ITEMS (group 2 should NOT be terminated)"
done

# group 1 must NOT have completed (terminated): its children should not have a
# full output file. (A terminated stream writes no file or a partial one.)
for child in details_a details_b; do
  n=$(count_records "$OUT_DIR/$child.json")
  echo "  group1 $child => $n records"
  [[ "$n" == "$NUM_ITEMS" ]] && fail "group1 $child produced full output ($n) — it should have been terminated"
done

echo "FAIL-FAST PATH OK: group 1 failed/terminated, independent group 2 succeeded"

echo "QUEUE STREAMING TEST PASSED"
