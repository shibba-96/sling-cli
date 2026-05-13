#!/usr/bin/env bash
# End-to-end ADBC SQL Server pipeline. Runs the full sequence from the README without
# any manual steps:
#
#   1. Start SQL Server 2022 + SeaweedFS S3 via docker compose, wait for health.
#   2. Install the ADBC SQL Server driver (dbc install mssql) if not already present.
#   3. Configure the MSSQL_ADBC + SEAWEED_S3 sling connections.
#   4. Run the CLI pipeline (p.41.adbc_sqlserver.yaml).
#   5. Run the Python script (run_pipeline.py) at 100k-row scale.
#   6. Tear the containers down on success (or on Ctrl-C).
#
# Usage:
#   ./run_pipeline.sh              # full run, then docker compose down -v
#   ./run_pipeline.sh --keep-up    # leave containers running for further inspection
#   ./run_pipeline.sh --cli-only   # skip the Python (uv) section
#   ./run_pipeline.sh --py-only    # skip the CLI pipeline section
#
# Exit non-zero on any failure. Safe to re-run (the pipeline and python both
# drop their target tables before reload).

set -euo pipefail

# --- Resolve paths ------------------------------------------------------------

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/../../../../../.." && pwd)"
PIPELINE_YAML="$HERE/p.41.adbc_sqlserver.yaml"
PY_SCRIPT="$HERE/run_pipeline.py"

KEEP_UP=0
RUN_CLI=1
RUN_PY=1

for arg in "$@"; do
    case "$arg" in
        --keep-up)  KEEP_UP=1 ;;
        --cli-only) RUN_PY=0 ;;
        --py-only)  RUN_CLI=0 ;;
        -h|--help)
            sed -n '2,/^set -euo/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *) echo "Unknown arg: $arg" >&2; exit 2 ;;
    esac
done

log() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m!! %s\033[0m\n' "$*" >&2; }
die() { printf '\033[1;31mFAIL: %s\033[0m\n' "$*" >&2; exit 1; }

# --- Preflight ----------------------------------------------------------------

command -v docker  >/dev/null || die "docker not on PATH"
docker compose version >/dev/null 2>&1 || die "docker compose v2 not available"
command -v sling   >/dev/null || die "sling CLI not on PATH (see https://docs.slingdata.io/sling-cli/getting-started)"
if [[ $RUN_PY -eq 1 ]]; then
    command -v uv  >/dev/null || die "uv not on PATH (brew install uv) — or rerun with --cli-only"
fi

# dbc (ADBC driver manager). Install if missing.
if ! command -v dbc >/dev/null; then
    log "dbc not found; installing ADBC driver manager"
    curl -LsSf https://dbc.columnar.tech/install.sh | sh
    # The installer drops dbc into $HOME/.local/bin by default — ensure it's on PATH for this run.
    export PATH="$HOME/.local/bin:$PATH"
    command -v dbc >/dev/null || die "dbc install reported success but dbc is still not on PATH; check ~/.local/bin"
fi

# ADBC mssql driver. Idempotent.
if ! dbc list 2>/dev/null | grep -qi '^mssql'; then
    log "Installing dbc mssql driver"
    dbc install mssql
fi

# --- Bring up containers ------------------------------------------------------

cd "$HERE"

log "Starting docker compose stack (mssql + seaweedfs)"
docker compose up -d

cleanup() {
    if [[ $KEEP_UP -eq 0 ]]; then
        log "Tearing down docker compose stack"
        (cd "$HERE" && docker compose down -v) || true
    else
        warn "--keep-up set; leaving sling-adbc-mssql + sling-adbc-seaweedfs running"
    fi
}
trap cleanup EXIT

# Wait for SQL Server health (compose healthcheck does the heavy lifting; poll for "healthy").
log "Waiting for SQL Server to become healthy"
for i in $(seq 1 60); do
    status=$(docker inspect sling-adbc-mssql --format='{{.State.Health.Status}}' 2>/dev/null || echo "starting")
    case "$status" in
        healthy)    echo "  mssql: healthy"; break ;;
        unhealthy)  die "sling-adbc-mssql reported unhealthy — inspect with: docker logs sling-adbc-mssql" ;;
        *)          printf '  waiting (status=%s, %ds)\n' "$status" "$((i*2))"; sleep 2 ;;
    esac
    [[ $i -eq 60 ]] && die "sling-adbc-mssql did not reach healthy within 120s"
done

# SeaweedFS is fast; just confirm the S3 port answers before we lean on it.
if [[ $RUN_PY -eq 1 ]]; then
    log "Waiting for SeaweedFS S3 endpoint"
    for i in $(seq 1 30); do
        if curl -sf -o /dev/null http://localhost:18333/ 2>/dev/null; then
            echo "  seaweedfs: responding"
            break
        fi
        sleep 1
        [[ $i -eq 30 ]] && die "SeaweedFS S3 endpoint (localhost:18333) did not respond within 30s"
    done
fi

# --- Configure sling connections ---------------------------------------------

log "Configuring sling connections (MSSQL_ADBC + SEAWEED_S3)"
sling conns set MSSQL_ADBC type=sqlserver \
    host=localhost port=51444 user=sa password='AdbcPipeline123!' \
    database=master encrypt=disable use_adbc=true >/dev/null

if [[ $RUN_PY -eq 1 ]]; then
    sling conns set SEAWEED_S3 type=s3 endpoint=http://localhost:18333 \
        bucket=pipeline access_key_id=any secret_access_key=any >/dev/null
fi

log "Testing MSSQL_ADBC"
sling conns test MSSQL_ADBC

if [[ $RUN_PY -eq 1 ]]; then
    log "Testing SEAWEED_S3"
    sling conns test SEAWEED_S3
fi

# --- CLI pipeline -------------------------------------------------------------

if [[ $RUN_CLI -eq 1 ]]; then
    log "Running CLI pipeline (p.41.adbc_sqlserver.yaml)"
    (cd "$REPO_ROOT" && sling run -p "$PIPELINE_YAML")
fi

# --- Python (uv) pipeline -----------------------------------------------------

if [[ $RUN_PY -eq 1 ]]; then
    log "Running Python pipeline (uv run run_pipeline.py)"
    (cd "$REPO_ROOT" && uv run "$PY_SCRIPT")
fi

log "ADBC SQL Server end-to-end pipeline PASSED"
