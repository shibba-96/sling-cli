# /// script
# requires-python = ">=3.10"
# dependencies = [
#   "sling[arrow]",
#   "duckdb",
#   "pyarrow",
#   "boto3",
# ]
# ///
"""ADBC SQL Server pipeline, Python edition.

End-to-end: DuckDB generates 100k rows and writes them as multiple parquet
files into a SeaweedFS S3 bucket. Sling reads from the S3 prefix and loads
into SQL Server via ADBC. Then we read back through ADBC using
`stream_arrow()` to get native Arrow types end-to-end.

Run with `uv` so you do not need to manage a virtualenv:

    uv run run_pipeline.py

Prereqs (one-time):

    docker compose up -d                     # SQL Server + SeaweedFS
    dbc install mssql                        # ADBC driver for SQL Server
    sling conns set MSSQL_ADBC type=sqlserver host=localhost port=51444 \\
        user=sa password='AdbcPipeline123!' database=master encrypt=disable use_adbc=true
    sling conns set SEAWEED_S3 type=s3 endpoint=http://localhost:18333 \\
        bucket=pipeline access_key_id=any secret_access_key=any
"""

from __future__ import annotations

import os

import boto3
import duckdb
import pyarrow as pa

from sling import Mode, Replication, ReplicationStream, Sling
from sling.hooks import HookQuery

# -- Config --------------------------------------------------------------------

MSSQL_CONN = "MSSQL_ADBC"
S3_CONN = "SEAWEED_S3"
S3_ENDPOINT = "http://localhost:18333"
S3_BUCKET = "pipeline"
S3_PREFIX = "adbc-py/"
PARQUET_FOLDER = f"s3://{S3_BUCKET}/{S3_PREFIX}"

ROWS_PER_FILE = 25_000
NUM_FILES = 4  # 100k rows total

# -- 1. Generate 100k rows in pyarrow, write as multiple parquet files to S3 ---

print(f"generating {NUM_FILES * ROWS_PER_FILE:,} rows in pyarrow...")

s3 = boto3.client(
    "s3",
    endpoint_url=S3_ENDPOINT,
    aws_access_key_id="any",
    aws_secret_access_key="any",
    region_name="us-east-1",
)

# Ensure the bucket exists (SeaweedFS auto-creates on first put, but this is explicit).
try:
    s3.create_bucket(Bucket=S3_BUCKET)
except s3.exceptions.BucketAlreadyOwnedByYou:
    pass
except Exception:
    pass

# Clear any prior run.
for obj in s3.list_objects_v2(Bucket=S3_BUCKET, Prefix=S3_PREFIX).get("Contents", []):
    s3.delete_object(Bucket=S3_BUCKET, Key=obj["Key"])

# Generate each file via DuckDB into a pyarrow table, then write parquet bytes via pyarrow.
con = duckdb.connect()
for n in range(NUM_FILES):
    start_id = n * ROWS_PER_FILE + 1
    end_id = start_id + ROWS_PER_FILE
    arrow_tbl: pa.Table = con.execute(
        f"""
        select
          i as id,
          'user_' || i::varchar as name,
          round(i * 1.5, 2) as score,
          'cat_' || ((i % 10)::varchar) as category
        from range({start_id}, {end_id}) t(i)
        """
    ).arrow()

    import io
    from pyarrow import parquet as pq

    buf = io.BytesIO()
    pq.write_table(arrow_tbl, buf, compression="snappy")
    buf.seek(0)
    key = f"{S3_PREFIX}part-{n+1:04d}.parquet"
    s3.upload_fileobj(buf, S3_BUCKET, key)
    print(f"  wrote s3://{S3_BUCKET}/{key} ({arrow_tbl.num_rows:,} rows)")

# -- 2. Replicate s3://bucket/prefix/ -> SQL Server via ADBC --------------------

print(f"\nreplicating {PARQUET_FOLDER} -> {MSSQL_CONN} via ADBC (full-refresh)...")

Replication(
    source=S3_CONN,
    target=MSSQL_CONN,
    streams={
        f"{S3_BUCKET}/{S3_PREFIX}": ReplicationStream(
            mode=Mode.FULL_REFRESH,
            object="dbo.adbc_pipeline_py",
        ),
    },
    hooks={
        "start": [
            HookQuery(
                connection=MSSQL_CONN,
                query=(
                    "if object_id('dbo.adbc_pipeline_py','U') is not null "
                    "drop table dbo.adbc_pipeline_py"
                ),
            ),
        ],
    },
).run()

# -- 3. Read back through ADBC with stream_arrow() for native Arrow types ------

print("\nreading back via ADBC stream_arrow() (first 5 rows by id)...")

stream = Sling(
    src_conn=MSSQL_CONN,
    src_stream="select top 5 id, name, score, category from dbo.adbc_pipeline_py order by id",
).stream_arrow()

batches = list(stream)
total_first5 = sum(b.num_rows for b in batches)
assert total_first5 == 5, f"expected 5 rows, got {total_first5}"
print(f"  got {len(batches)} arrow batch(es), {total_first5} rows")
print(f"  schema: {batches[0].schema}")
for row in pa.Table.from_batches(batches).to_pylist():
    print(f"  {row}")

# -- 4. Verify the row count via ADBC ------------------------------------------

count_stream = Sling(
    src_conn=MSSQL_CONN,
    src_stream="select count(*) as cnt from dbo.adbc_pipeline_py",
).stream_arrow()
count_table = pa.Table.from_batches(list(count_stream))
total = count_table.to_pylist()[0]["cnt"]
print(f"\ntotal rows in dbo.adbc_pipeline_py: {total:,}")
assert total == NUM_FILES * ROWS_PER_FILE, (
    f"expected {NUM_FILES * ROWS_PER_FILE} rows, got {total}"
)

# -- 5. Cleanup ----------------------------------------------------------------

Sling(
    src_conn=MSSQL_CONN,
    src_stream="drop table dbo.adbc_pipeline_py",
).run()

print("\nADBC SQL Server Python pipeline PASSED")
