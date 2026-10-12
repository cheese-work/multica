# Pull Requests

Product contracts the runtime brief does not fully encode.

- [Reading a linked PR's real state](#reading-a-linked-prs-real-state)

## Reading a linked PR's real state

When a step depends on PR state, query Multica's link table — do not infer it
from branch names, GitHub search, memory, or stale values left on the issue by
an earlier run.

```bash
multica issue pull-requests <issue-id> --output json
```

Returns `{"pull_requests": [...], ...}`. Each element of `pull_requests` exposes:

- `number`, `html_url`, `title`
- `link_source` — why the PR is on the issue: `title`, `branch`, `manual`, or
  `auto` (any other automatic link, such as a closing keyword in the body).
- `state` — the PR lifecycle as a **single enum**, one of `merged`, `closed`,
  `draft`, `open`. There is no separate `draft` or `merged` boolean in the
  response; the server folds them into `state` (merged wins, then closed, then
  draft, else open).
- `merged_at` — non-null once merged; a second confirmation of `state: merged`.
- `provider` — `github`, `forgejo`, `gitea`, or `gitlab`.
- `mergeable_state` — mirrors GitHub (`clean` / `dirty` surfaced; other values
  round-trip as unknown; retained for compatibility).
- GitHub API snapshot fields: `snapshot_available`, `mergeable`,
  `merge_state_status`, `checks_rollup`, `checks_total`, `checks_passed`,
  `checks_failed`, `checks_running`, `failed_check_names`,
  `snapshot_fetched_at`, and `snapshot_stale`. `snapshot_available == true`
  means the feature is enabled and the snapshot matches the PR's current head.
  Only then does `checks_rollup == null` mean "no checks"; false means the
  snapshot feature is disabled, has not fetched yet, or only has an old head.
- `checks_conclusion` — coarse CI compatibility status: `passed`, `failed`,
  `pending`, or `null`. GitHub derives it from the current API snapshot;
  Forgejo/Gitea/GitLab derive it from webhook commit statuses. Backed by the
  provider-appropriate check counts.

So "is it merged?" is `state == "merged"` (or `merged_at != null`); "is it still
a draft?" is `state == "draft"`; coarse CI status is `checks_conclusion`.

`--output table` (the default) adds three derived columns on top of `NUMBER` /
`STATE` / `TITLE` / `URL`:

- `HEAD` — the PR's `branch`, or `unavailable` when absent. The response does
  not expose a head commit SHA (only branch), so this column identifies the
  head by branch name, not by commit.
- `CI` — `unavailable` when no current snapshot exists (`snapshot_available`
  is not `true`); `no checks` when a snapshot exists but `checks_rollup` is
  `null` (checks have not reported yet — **never** rendered as `passed`);
  otherwise the raw `checks_rollup` value.
- `SNAPSHOT` — `unavailable` with no snapshot; `stale` (with an age) when
  `snapshot_stale` is `true`; otherwise the age since `snapshot_fetched_at`.
