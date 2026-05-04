#!/bin/bash
# Self-contained PostGIS reproduction test.
# Spins up postgis/postgis via docker compose, exposes it as the POSTGIS
# Sling connection, runs the pipeline, then tears the container down.
#
# Usage: bash run_docker_test.sh
# Requires: Docker (with compose v2), a built sling binary at cmd/sling/sling
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
  echo "=== Tearing down PostGIS container ==="
  docker compose -f "$SCRIPT_DIR/docker-compose.yaml" down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "=== Starting PostGIS container ==="
docker compose -f "$SCRIPT_DIR/docker-compose.yaml" up -d --wait

# The postgis image bootstraps once on first start: postgres comes up, runs the
# init SQL that loads the extensions, then shuts postgres down and restarts it.
# pg_isready / a single SELECT can succeed during the init phase only to be
# refused seconds later. Wait until the bootstrap log line appears and then
# require N consecutive successful SQL probes from the host.
echo "=== Waiting for PostGIS bootstrap to finish ==="
for i in $(seq 1 60); do
  if docker logs sling-test-postgis 2>&1 | grep -q "database system is ready to accept connections" \
     && docker logs sling-test-postgis 2>&1 | grep -q "PostgreSQL init process complete"; then
    break
  fi
  if [ "$i" = "60" ]; then
    echo "ERROR: PostGIS init did not finish in time"
    docker logs sling-test-postgis | tail -40
    exit 1
  fi
  sleep 1
done

echo "=== Waiting for stable SQL connections from host ==="
stable=0
for i in $(seq 1 60); do
  if docker exec sling-test-postgis psql -U postgres -d postgis_test -tAc "SELECT 1" >/dev/null 2>&1; then
    stable=$((stable + 1))
    if [ "$stable" -ge 3 ]; then
      echo "PostGIS stable."
      break
    fi
  else
    stable=0
  fi
  if [ "$i" = "60" ]; then
    echo "ERROR: PostGIS did not become stable in time"
    docker logs sling-test-postgis | tail -40
    exit 1
  fi
  sleep 1
done

# Expose container as the POSTGIS Sling connection for this run only.
export POSTGIS='postgres://postgres:postgres@127.0.0.1:55432/postgis_test?sslmode=disable'

echo "=== Running PostGIS pipeline test ==="
"$SLING_BIN" run -d -p "$SCRIPT_DIR/p.41.postgis_geometry.yaml"

echo "=== PostGIS reproduction test completed ==="
