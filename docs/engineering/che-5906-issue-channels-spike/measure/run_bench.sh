#!/usr/bin/env bash
# usage: run_bench.sh <label> <days> [sqlfile]   60 runs on one connection; prints p50/p95/max in ms
set -euo pipefail
label="$1"; days="$2"; f="${3:-measure/channel_list.sql}"
sed "s/:'w'/'00000000-0000-0000-0000-00000000aaaa'/g; s/:'m'/'00000000-0000-0000-0000-000000000001'/g; s/:days/$days/g" "$f" > /tmp/q.$$.sql
docker cp /tmp/q.$$.sql che5906-pg:/tmp/q.sql
docker exec -e PGOPTIONS="${PGOPTIONS:-}" che5906-pg bash -c "rm -f /tmp/pgbench_log*; pgbench -U multica -d multica_bench -n -f /tmp/q.sql -c 1 -t ${RUNS:-60} -l --log-prefix=/tmp/pgbench_log >/dev/null; awk '{print \$3/1000}' /tmp/pgbench_log* | sort -n" > /tmp/lat.$$
python3 -I - "$label" /tmp/lat.$$ <<'PY'
import sys
v=[float(x) for x in open(sys.argv[2])]; n=len(v)
print(f"{sys.argv[1]}: n={n} p50={v[n//2]:.1f}ms p95={v[int(n*.95)-1]:.1f}ms max={v[-1]:.1f}ms")
PY
rm -f /tmp/lat.$$ /tmp/q.$$.sql
