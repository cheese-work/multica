-- Repeats from several runtimes of one daemon, or from HTTP and WS, hit the
-- unique key and are dropped. CPU/RAM use mount_path = ''; each mount is a row.

-- name: InsertHostSampleCPURAM :exec
INSERT INTO host_sample (daemon_id, sampled_at, mount_path, cpu_percent, ram_percent)
VALUES ($1, $2, '', $3, $4)
ON CONFLICT (daemon_id, sampled_at, mount_path) DO NOTHING;

-- name: InsertHostSampleDisk :exec
INSERT INTO host_sample (daemon_id, sampled_at, mount_path, disk_percent)
VALUES ($1, $2, $3, $4)
ON CONFLICT (daemon_id, sampled_at, mount_path) DO NOTHING;

-- name: GetLatestHostSampleAt :one
SELECT sampled_at FROM host_sample WHERE daemon_id = $1 ORDER BY sampled_at DESC LIMIT 1;

-- name: MaxHostSampleHourlyBucket :one
SELECT max(bucket_ts)::timestamptz AS bucket_ts FROM host_sample_hourly;

-- name: RollupHostSampleHourly :execrows
-- Folds raw rows in [from_ts, to_ts) into hourly rows per (daemon, mount).
-- Idempotent: a rerun recomputes from the raw rows still present. Hours with
-- no raw row are not written (gap, not zero).
INSERT INTO host_sample_hourly (daemon_id, bucket_ts, mount_path,
    cpu_max, cpu_avg, ram_max, ram_avg, disk_max, disk_avg, sample_count, expected_count)
SELECT daemon_id, date_trunc('hour', sampled_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS hour_ts, mount_path,
    max(cpu_percent), avg(cpu_percent)::real, max(ram_percent), avg(ram_percent)::real, max(disk_percent), avg(disk_percent)::real,
    count(*)::int, 60
FROM host_sample
WHERE sampled_at >= @from_ts::timestamptz AND sampled_at < @to_ts::timestamptz
GROUP BY daemon_id, hour_ts, mount_path
ON CONFLICT (daemon_id, bucket_ts, mount_path) DO UPDATE SET
    cpu_max = EXCLUDED.cpu_max,
    cpu_avg = EXCLUDED.cpu_avg,
    ram_max = EXCLUDED.ram_max,
    ram_avg = EXCLUDED.ram_avg,
    disk_max = EXCLUDED.disk_max,
    disk_avg = EXCLUDED.disk_avg,
    sample_count = EXCLUDED.sample_count;

-- name: DeleteHostSampleBefore :execrows
DELETE FROM host_sample WHERE sampled_at < @cutoff::timestamptz;

-- name: DeleteHostSampleHourlyBefore :execrows
DELETE FROM host_sample_hourly WHERE bucket_ts < @cutoff::timestamptz;
