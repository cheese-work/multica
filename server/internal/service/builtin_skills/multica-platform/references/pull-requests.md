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

## Listing and unlinking

```bash
multica issue pull-requests list <issue-id> --output json
multica issue pull-requests unlink <issue-id> <pr>
```

`<pr>` is a pull request UUID, a number (`42` or `#42`), `owner/repo#number`, or
a URL. `unlink` removes the link and records an exclusion, so later webhooks for
that pull request do not re-link it or auto-complete the issue. It is the
supported way to detach a PR that should not drive this issue; it is not undone
by pushing to the PR. Unlink only when the link is wrong.

## Guard

- `multica guard check-pr <pr-url|number|branch> [-R owner/name]` reads a pull
  request back and checks it against the workspace policy and the repository's
  `.github/agent-rules.yml`, read from the current tip of the PR's protected
  base branch (never the PR branch). It edits nothing and is meant as a CI
  check. Exit 0: passes. Exit 1: breaks a rule. Exit 2: the guard refuses (a
  malformed rules file, an unreadable base branch, a locked-down policy).
- `multica guard exec <tool> -- <args...>` runs a tool after the guard checked,
  fixed or refused the call. It covers `gh pr create|edit|ready` and writes to
  pull requests and issues through `gh api`. Exit code: the tool's own, 2 when
  the guard refuses. Environment: `MULTICA_GUARD_POLICY` and
  `MULTICA_GUARD_POLICY_SHA256` (signed policy file and digest),
  `MULTICA_GUARD_LOG` (one JSON line per decision), `MULTICA_GUARD_REAL_<TOOL>`
  (path of the real binary, for example `MULTICA_GUARD_REAL_GH`).
- `multica guard plugin list` lists the plugins of every scope in the stored
  policy. `multica guard plugin get <name>` shows every scope's definition of
  one plugin.
- `multica guard plugin set --file <policy.json> [--dry-run] [--out <path>] [--expires-in <s>]`
  validates a policy document and writes the action file for
  `multica decision request --action-file`. Nothing is sent and the stored
  policy does not change until the decision is approved. `--dry-run` prints both
  digests and the action. Deeper layers override shallower ones key by key; a
  locked layer freezes the keys it sets.
