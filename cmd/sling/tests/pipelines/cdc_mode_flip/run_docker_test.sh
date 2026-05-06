#!/bin/bash
# Self-contained reproduction for the CDC mode-flip bug.
# (mode: change-capture -> mode: full-refresh -> mode: change-capture)
#
# Spins up a MySQL 8 container with binlog/GTID enabled, exposes it as the
# MYSQL_CDC_FLIP Sling connection, runs the pipeline, then tears the container
# down. The Snowflake side uses the existing SNOWFLAKE connection.
#
# Usage: bash run_docker_test.sh
# Requires:
#   - docker (with compose v2)
#   - a built sling binary at sling-cli/cmd/sling/sling (or it will build one)
#   - a working SNOWFLAKE connection in `sling conns list`

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SLING_CLI_DIR="$(cd "$SCRIPT_DIR/../../../../.." && pwd)"
CMD_DIR="$SLING_CLI_DIR/cmd/sling"
SLING_BIN="$CMD_DIR/sling"

if [ ! -x "$SLING_BIN" ]; then
  echo "Building sling binary..."
  (cd "$CMD_DIR" && go build .)
fi

cleanup() {
  echo "=== Tearing down MySQL container ==="
  docker compose -f "$SCRIPT_DIR/docker-compose.yaml" down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "=== Starting MySQL container (GTID + binlog) ==="
docker compose -f "$SCRIPT_DIR/docker-compose.yaml" up -d --wait

echo "=== Waiting for MySQL to be ready ==="
for i in $(seq 1 60); do
  if docker exec sling-cdc-flip-mysql mysqladmin ping -h 127.0.0.1 -u root -psling_cdc_test >/dev/null 2>&1; then
    # also verify the cdc_flip_db database is reachable
    if docker exec sling-cdc-flip-mysql mysql -u sling_repl -psling_repl_pass -e "SELECT 1" cdc_flip_db >/dev/null 2>&1; then
      echo "MySQL ready."
      break
    fi
  fi
  if [ "$i" = "60" ]; then
    echo "ERROR: MySQL did not become ready in time"
    docker logs sling-cdc-flip-mysql | tail -60
    exit 1
  fi
  sleep 1
done

# Verify GTID mode is on (CDC reader uses GTID for shared-reader mode)
gtid_mode=$(docker exec sling-cdc-flip-mysql mysql -u root -psling_cdc_test -N -B -e "SELECT @@gtid_mode" 2>/dev/null || echo "")
echo "GTID mode: $gtid_mode"

# Expose the container as the MYSQL_CDC_FLIP Sling connection for this run
export MYSQL_CDC_FLIP='mysql://sling_repl:sling_repl_pass@127.0.0.1:3399/cdc_flip_db?allowNativePasswords=true&parseTime=true'

# Sling CDC requires SLING_STATE — point it at SNOWFLAKE.PUBLIC.SLING_STATE.
export SLING_STATE='SNOWFLAKE/PUBLIC.SLING_STATE'

echo "=== Verifying SNOWFLAKE connection ==="
"$SLING_BIN" conns test SNOWFLAKE

echo "=== Running CDC mode-flip pipeline ==="
"$SLING_BIN" run -d -p "$SCRIPT_DIR/p.cdc_mode_flip.yaml"

echo "=== CDC mode-flip reproduction completed ==="
