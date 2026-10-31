-- One daemon host sample per minute. The CPU/RAM row has mount_path = ''; each
-- mounted filesystem adds one row with disk_percent. No foreign key to
-- agent_runtime: history must outlive a deleted runtime.
CREATE TABLE host_sample (
    daemon_id    TEXT NOT NULL,
    sampled_at   TIMESTAMPTZ NOT NULL,
    mount_path   TEXT NOT NULL DEFAULT '',
    cpu_percent  REAL,
    ram_percent  REAL,
    disk_percent REAL,
    CHECK (
        (mount_path = '' AND cpu_percent IS NOT NULL AND ram_percent IS NOT NULL AND disk_percent IS NULL)
        OR (mount_path <> '' AND disk_percent IS NOT NULL AND cpu_percent IS NULL AND ram_percent IS NULL)
    )
);
