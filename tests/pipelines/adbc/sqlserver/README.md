# ADBC SQL Server pipeline test

End-to-end check that Sling can write to and read from SQL Server through the
[ADBC](https://arrow.apache.org/adbc/) driver, including the bulk-load path
from local parquet files (CLI pipeline) and from an S3 prefix (Python).

## What's in here

| File | Purpose |
|---|---|
| `docker-compose.yaml` | SQL Server 2022 on `localhost:51444` + SeaweedFS (S3) on `localhost:18333` |
| `p.41.adbc_sqlserver.yaml` | CLI pipeline: duckdb -> local parquet -> ADBC -> SQL Server, with read-back + assertions |
| `run_pipeline.py` | Python equivalent at 100k-row scale: duckdb -> pyarrow parquet -> SeaweedFS S3 -> ADBC -> SQL Server, then read back with `stream_arrow()` |
| `run_pipeline.sh` | One-shot end-to-end driver: brings up docker compose, installs the ADBC mssql driver, configures connections, runs the CLI pipeline and the Python pipeline, tears down |

The CLI pipeline keeps the parquet local (simplest reproduction). The Python
script uses S3 (SeaweedFS) to exercise the more common production shape: many
parquet files in object storage, picked up by a single Sling replication.

## Prerequisites

- Docker (compose v2)
- Sling CLI on PATH (`brew install slingdata-io/sling/sling` or
  [other install methods](https://docs.slingdata.io/sling-cli/getting-started))
- [`duckdb`](https://duckdb.org/) CLI for the CLI pipeline (`brew install duckdb`)
- [`uv`](https://docs.astral.sh/uv/) for the Python script (`brew install uv`)
- The `dbc` ADBC driver manager:
  ```bash
  curl -LsSf https://dbc.columnar.tech/install.sh | sh
  dbc install mssql
  ```

## Run

One-shot end-to-end (recommended):

```bash
cd tests/pipelines/adbc/sqlserver
./run_pipeline.sh                  # full run, then docker compose down -v
./run_pipeline.sh --keep-up        # leave containers running for inspection
./run_pipeline.sh --cli-only       # skip the Python (uv) section
./run_pipeline.sh --py-only        # skip the CLI pipeline
```

Manual sequence (if you want to step through):

```bash
cd tests/pipelines/adbc/sqlserver
docker compose up -d
# wait for SQL Server to become healthy (~10–20s)
docker inspect sling-adbc-mssql --format='{{.State.Health.Status}}'

# Configure the SQL Server connection (one-time)
sling conns set MSSQL_ADBC type=sqlserver \
  host=localhost port=51444 user=sa password='AdbcPipeline123!' \
  database=master encrypt=disable use_adbc=true
sling conns test MSSQL_ADBC

# Configure the SeaweedFS connection used by run_pipeline.py
sling conns set SEAWEED_S3 type=s3 endpoint=http://localhost:18333 \
  bucket=pipeline access_key_id=any secret_access_key=any
sling conns test SEAWEED_S3

# --- CLI pipeline (local parquet) ---
cd ../../../../..
sling run -p tests/pipelines/adbc/sqlserver/p.41.adbc_sqlserver.yaml

# --- Python (S3 + 100k rows + stream_arrow) ---
uv run tests/pipelines/adbc/sqlserver/run_pipeline.py
```

Stop and remove the containers when done:

```bash
cd tests/pipelines/adbc/sqlserver
docker compose down -v
```

## Notes

- Port `51444` is used to avoid colliding with the dev SQL Server instances on
  `51433` / `51443`.
- The `use_adbc: true` flag on a regular `type: sqlserver` connection routes
  reads and writes through the ADBC driver. The non-ADBC (TDS) path still works
  against the same connection if you flip the flag off.
- SeaweedFS `server -s3` runs anonymous-write by default; the
  `access_key_id` / `secret_access_key` on the Sling side are accepted by the
  S3 client library but not actually verified by the server. For production,
  configure credentials with `weed shell` and `s3.configure`.
- The `mcr.microsoft.com/mssql/server:2022-latest` image is published as
  linux/amd64 only; Apple Silicon hosts run it under emulation.
