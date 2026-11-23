#!/usr/bin/env python3
"""Redact the OOM-killer records of an X99 boot into the 9a replay fixture.

Input:  `journalctl -k -b -o json` on stdin (one JSON entry per line).
Output: JSON lines on stdout, the first N kill events in journal order.

Per event we keep only what the classifier reads plus two noise lines:
the `invoked oom-killer` header, the `Memory cgroup stats for` line (when
present), one task-table row, the `oom-kill:constraint=` line and the
`Killed process` record. Redaction: process names become `proc`, each
distinct 64-hex container ID becomes another stable 64-hex value, the
boot ID is zeroed, cursors are synthetic. PIDs, UIDs, cgroup paths of
systemd units and the timestamps stay (the join and the key rules need
them). Usage: redact_boot_oom.py 129 < boot-kernel.jsonl > fixture.jsonl
"""
import hashlib
import json
import re
import sys

N = int(sys.argv[1]) if len(sys.argv) > 1 else 129
HEX64 = re.compile(r"[0-9a-f]{64}")
BOOT = "00000000000000000000000000000000"


def msg(e):
    m = e["MESSAGE"]
    return m if isinstance(m, str) else bytes(m).decode("utf8", "replace")


def scrub_ids(s):
    return HEX64.sub(lambda m: hashlib.sha256(b"che-7238:" + m.group(0).encode()).hexdigest(), s)


def scrub(s):
    s = scrub_ids(s)
    # header: "<comm> invoked oom-killer: ..."
    s = re.sub(r"^.* invoked oom-killer:", "proc invoked oom-killer:", s)
    # constraint line: task=<comm>,pid=<n>,uid=<n> (comm may hold commas and parens)
    s = re.sub(r",task=.*,pid=(\d+),uid=(\d+)$", r",task=proc,pid=\1,uid=\2", s)
    # record: "Killed process <pid> (<comm>) total-vm..."
    s = re.sub(r"Killed process (\d+) \(.*?\) total-vm", r"Killed process \1 (proc) total-vm", s)
    # task-table row: keep the numbers, replace the trailing name
    s = re.sub(r"^(\[\s*\d+\].*\s)\S.*$", r"\1proc", s) if s.startswith("[") and "pid" not in s[:12] else s
    return s


ents = [json.loads(l) for l in sys.stdin]
out, kills, i = [], 0, 0
events = []
cur = None
for e in ents:
    m = msg(e)
    if " invoked oom-killer:" in m and not m.startswith("["):
        cur = [e]
        continue
    if cur is None:
        continue
    cur.append(e)
    if "Killed process" in m and not m.startswith("["):
        events.append(cur)
        cur = None
        if len(events) == N:
            break

seq = 0
for ev in events:
    keep = []
    for e in ev:
        m = msg(e)
        if (
            " invoked oom-killer:" in m
            or m.startswith("Memory cgroup stats for")
            or m.startswith("oom-kill:constraint=")
            or "Killed process" in m
            or (m.startswith("[") and "pid" not in m[:12])
        ):
            keep.append(e)
    # at most one task-table row (the last one before the constraint line)
    rows = [e for e in keep if msg(e).startswith("[")]
    if len(rows) > 1:
        keep = [e for e in keep if not msg(e).startswith("[") or e is rows[-1]]
    for e in keep:
        seq += 1
        out.append(
            {
                "MESSAGE": scrub(msg(e)),
                "PRIORITY": e.get("PRIORITY", "6"),
                "SYSLOG_IDENTIFIER": "kernel",
                "_BOOT_ID": BOOT,
                "_TRANSPORT": "kernel",
                "__MONOTONIC_TIMESTAMP": e["__MONOTONIC_TIMESTAMP"],
                "_SOURCE_MONOTONIC_TIMESTAMP": e.get("_SOURCE_MONOTONIC_TIMESTAMP", e["__MONOTONIC_TIMESTAMP"]),
                "__CURSOR": "s=00000000000000000000000000000000;i=%x;b=%s;m=%x;t=0;x=0"
                % (seq, BOOT, int(e["__MONOTONIC_TIMESTAMP"])),
            }
        )
for o in out:
    print(json.dumps(o, sort_keys=True))
