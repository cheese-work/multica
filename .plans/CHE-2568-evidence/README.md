# CHE-2568 evidence

Aggregates only. No run output, handoff note, chat content or comment text is published here.

| File | Content |
| --- | --- |
| `aggregates.json` | window; runs, USD and Mtok per trigger class and run class (7,767 runs) |
| `manifest.json` | window; sha256 of the tasks file and of each of 954 issue timelines, for a holder of the same data |
| `g1_records.json` | the 122 runs the bare G1 predicate matches: run ID, issue ID, run class, USD, eligibility facts, shadow verdict |

The aggregates came from a one-off fetch on 1 Nov 2026 by scripts that are not in this PR. They cannot be reproduced from these files, and the server data is mutable. `g1_records.json` was built by calling `CHE-2568-scripts/g1.py` `evaluate` on each matched issue's pull requests, timeline and `comment list --full`; `tally` over it gives the figures in the plan.
