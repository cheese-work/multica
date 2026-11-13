-- Hourly rollup of host_sample (CHE-2586 L2b). Same key shape as the raw table:
-- the CPU/RAM row has mount_path = '', each mount adds one row. max feeds the
-- 80% threshold line, avg the trend. sample_count < expected_count (60) marks a
-- partial hour; an hour with no raw row is a gap and is not written. No foreign
-- key: history must outlive a deleted runtime.
CREATE TABLE IF NOT EXISTS host_sample_hourly (
    daemon_id      TEXT        NOT NULL,
    bucket_ts      TIMESTAMPTZ NOT NULL,
    mount_path     TEXT        NOT NULL DEFAULT '',
    cpu_max        REAL,
    cpu_avg        REAL,
    ram_max        REAL,
    ram_avg        REAL,
    disk_max       REAL,
    disk_avg       REAL,
    sample_count   INTEGER     NOT NULL,
    expected_count INTEGER     NOT NULL DEFAULT 60,
    PRIMARY KEY (daemon_id, bucket_ts, mount_path),
    CHECK (
        (mount_path = '' AND cpu_max IS NOT NULL AND ram_max IS NOT NULL AND disk_max IS NULL)
        OR (mount_path <> '' AND disk_max IS NOT NULL AND cpu_max IS NULL AND ram_max IS NULL)
    )
);
