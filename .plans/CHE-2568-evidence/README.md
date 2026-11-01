# CHE-2568 evidence (window 25 Oct 2026 07:00 ICT to 1 Nov 2026 22:00 ICT)

| File | What it is | sha256 |
| --- | --- | --- |
| `rows.json.gz` | one row per completed run (7,767): class, run class, cost, tokens, trigger facts, first 400 chars of output | `2e432730…` (first 8 hex digits of the gz) |
| `manifest.json` | fetch manifest: window, sha256 of `tasks.json` and of each of 954 issue timelines | `b7f17069…` |
| `g1_evidence.json` | the 122 G1-matched runs: eligibility facts and shadow verdict | `b0eb8e1c…` |

Not published: raw `tasks.json` (27 MB, sha256 `689fab04…`) and the timelines. Both are mutable on the server, so a later fetch will not match the manifest hashes.

Reproduce from the published rows (no network):

    mkdir work && cd work && zcat ../rows.json.gz > rows2.json
    python3 ../../CHE-2568-scripts/est.py     # run classes and no-change totals
    python3 ../../CHE-2568-scripts/gates.py   # G1, G2, G3, G4 rows

`gates.py` rows for G5 and G6 read timelines and print 0 from the published rows. Their figures in the plan come from the original fetch and are not reproducible from this evidence. They are not used for any decision: both gates are dropped.
`g1_report.py report` also needs the per-issue fetch (`g1_report.py fetch`) and gives different numbers if issues changed since 1 Nov 2026.
