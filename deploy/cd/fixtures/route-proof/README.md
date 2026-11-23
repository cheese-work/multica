# route-proof fixtures (CHE-9276, plan CHE-9208 step 2)

Recorded from the GitHub API on 23 Nov 2026 (`gh api repos/cheese-work/multica/actions/runs/ID[/jobs]`, trimmed to the fields the script reads):

- `run-45069060254.json`, `jobs/45069060254.json`: cancelled run, zero jobs (the case that stalled CD qualification).
- `run-45066486281.json`, `jobs/45066486281.json`: cancelled run replaced by the concurrency group, zero jobs.
- `run-45060386606.json`, `jobs/45060386606.json`: failed run; its first two `build-images` legs are `success` (the API listed 5 jobs; the file keeps `build-images` only).
- `run-45114280355.json`, `jobs/45114280355.json`: completed `success` run with the old selector `self-hosted,linux,x64,cheese-x99` (wrong labels for a homedisk proof).

Synthetic (no real run has the homedisk selector yet), shaped like the recorded ones:

- `run-45999999001.json`, `jobs/45999999001.json`: completed run, both legs `success`, labels `self-hosted,linux,x64,cheese-x99,cheese-x99-homedisk`.
- `runners.json`: org runners list shaped like `gh api orgs/cheese-work/actions/runners` (read-only labels are capitalised there, e.g. `X64`).
