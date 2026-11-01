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
