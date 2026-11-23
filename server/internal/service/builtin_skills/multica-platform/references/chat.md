# Chat conversations

`multica chat` works on the conversation the current run belongs to: a web DM
or a channel thread. It cannot read any other session or channel.

```bash
multica chat history --limit 20 --output json   # channel overview: top-level messages + thread list
multica chat thread --output json               # the thread you were mentioned in
multica chat thread <thread-id> --output json   # a thread_id taken from history
```

- `history` is the table of contents: recent top-level messages and, per
  thread, `thread_id`, `reply_count` and `latest_reply`. It does not expand
  threads.
- Both reads page with `--limit` (the server clamps it) and `--before <cursor>`
  where the cursor is a `next_cursor` from the previous page.
- Same commands for every channel (web, Slack, ...).

## Continue a long DM

When a DM has grown too long, move it to a fresh session:

```bash
multica chat continue --summary-file ./summary.md           # archives this DM, opens a new one seeded with the summary
multica chat continue --fork                                # keeps this DM, opens a child DM with a copy of its messages
```

- Without `--fork`: this DM is archived and the conversation moves to a new
  DM seeded with the summary. Put what the next session needs in the summary:
  it is all the new session starts with.
- With `--fork`: this DM stays active and unchanged. The child gets a copy of
  the visible messages and attachments, no summary, no message sent.
  `--summary-file` is not accepted with `--fork`.
- A retry of the same turn returns the same child (`--idempotency-key`
  defaults to the current chat task id).

To keep the same DM instead, compact it in place:

```bash
multica chat compact --summary-file ./summary.md            # same DM id, history and URL; next turn starts from the summary
```

- The DM is never archived and no new DM is created. The summary is all the
  next turn starts with.
- If the server does not support in-place compaction, the command stops and
  the DM is unchanged.

The compaction task runs with the workspace's compact agent; an owner or admin
sets it (task tokens are refused), and with none set the DM's own agent runs it:

```bash
multica workspace update <workspace> --compact-agent <agent-id-or-name>   # name resolved in that workspace
multica workspace update <workspace> --compact-agent null                 # reset
```

- Alias for `multica workspace background-agents set compact`; exclusive with
  every other `workspace update` flag. The CLI reads the value back and fails
  with `compact agent was not saved` when the server kept another one.
- A compaction task cannot fork or archive the DM, and only the current
  compact agent may finish it. `multica chat compact` / `continue` then print
  the stable code and message on stderr and exit 3:
  `compaction_agent_not_current` (this agent is no longer the compact agent),
  `compaction_continue_refused` (finish with `chat compact`, not `continue`).

## Rename a DM

```bash
multica chat rename <session-id> <title> --output json
```

Sets the DM title and posts a message announcing the new title and who
changed it, without reading the DM's contents. JSON carries `id`, `title`,
`previous_title` and `message_id`.

## Declare the turn outcome

```bash
multica chat reply-state question --ask <ask-id>   # this turn answers/keeps one open ask
multica chat reply-state question --new            # this turn opens a new ask
multica chat reply-state done --ask <ask-id> [--ask <ask-id> ...]   # these asks are resolved
multica chat reply-state info                      # nothing is owed to you
```

Runs from a DM task and tells the server what the turn leaves the human
owing. The chat prompt lists the DM's open ask ids, oldest first, or says
"no open asks". `done` needs at least one `--ask`; without it the CLI refuses
and sends no request. `question` with two open asks needs `--ask <id>` or
`--new`; otherwise the server answers 400 and the CLI prints that text.

## Unseen DMs

```bash
multica chat unseen --output json        # DMs with unread messages; each row has unseen: true
multica chat mark-unseen <session-id>    # moves the read cursor so the DM shows unread again
```

`mark-unseen` is creator-only and a no-op when the DM has no assistant
message. `unseen` lists your own DMs through the DM list endpoint. It works
with an interactive sign-in, a PAT or a CLI sign-in and returns metadata only
(no message preview). A task token gets a 403. `multica chat sessions` rows
also carry `unseen` (JSON) and an `UNSEEN` column (table).

## Count progress cards in a member's DMs

```bash
multica chat count --member <user-id> --kind progress-card --since 2026-11-23T00:00:00Z --output json
# {"count": 3, "since": "...", "until": "..."}
```

Count only: no message text, session id or other member's data. A member counts
their own DMs; an agent run counts for the named member only when the run is the
member's or the calling agent is owned by the member. Anyone else gets 403.
Feed flow metric 7 (`FLOW_METRICS_DM_JSON`, a list of `{content, created_at}`)
with: `jq --arg s "$SINCE" '[range(.count) | {content: "Progress update", created_at: $s}]'`.

## Schedule a wakeup in this DM

```bash
multica chat wakeup create --after 25m --instruction "check the deploy and report" --output json
multica chat wakeup create --at 2026-11-01T09:00:00+07:00 --instruction "..." --output json
multica chat wakeup create --issue CHE-1 --event comment.created --filter-actor-type agent \
  --filter-actor-id <agent-id> --expires-in 2h --on-timeout wake --instruction "relay the reply" --output json
multica chat wakeup create --issue CHE-1 --until-status in_review --instruction "..." --output json
multica chat wakeup list --output json                  # pending wakeups of the current DM
multica chat wakeup cancel <wakeup-id>
```

- Give exactly one of `--after <duration>` (`25m`, `2h`) or `--at <RFC3339>`.
  `--instruction` is required: it is what the scheduled turn does when it fires.
- `--issue <id>` replaces `--after`/`--at`: the DM wakes once when the issue gets
  a `--event comment.created` (narrow it with `--filter-actor-type member|agent`
  and `--filter-actor-id`) or reaches `--until-status`. Give exactly one of the
  two. The refresh turn quotes the comment (id, author) or the new status, so
  you can relay it without the human asking. Matching is checked about every 30s.
- A watch gives up after `--expires-in` (at most 24h, default 24h). `--on-timeout
  wake` sends a "timed out" turn; the default `drop` ends it silently. You and
  the DM's human must be able to read the issue. It counts toward the 3-pending
  cap and shows in the Scheduled chip, where the human can cancel it.
- The wakeup starts a new turn in this DM later; finish the current turn
  instead of waiting for it.
- These are DM wakeups, including the issue watch above. To start an agent run
  on an issue, use `multica issue wakeup` (see [wakeups.md](wakeups.md)).

## DM lanes for decisions

`multica chat sessions --issue <issue> --for <user-id>` lists the DM lanes a
decision request can use. See [decisions.md](decisions.md).

## Waiting DMs and snooze

- `multica chat attention [--stale] [--output json]` lists the DMs that wait
  for you, oldest first. `--stale` keeps only those past 4 h (yellow) or 24 h
  (red).
- `multica chat later <session> [duration|time] [--request-id <uuid>]` snoozes
  a DM. A duration is `90m`, `2h`, `1d12h` or seconds; a time is RFC3339. The
  server rejects values outside 1 s to 168 h. Retry a failed call with the same
  `--request-id` so the retry does not extend the time.

## Closed DM segments

A DM run reads only the open segment. These two commands read earlier, closed
ones. The task token names the DM, so there is no session argument; another
DM's divider id is not found.

- `multica chat segments [--output table|json]` lists the closed segments of
  this DM, oldest first: divider id, when it was written, message count.
- `multica chat segment <divider-id> [--limit N] [--before <next_before>]`
  reads one segment, newest page first. When `has_more` is true the reply
  carries `next_before`; pass it as `--before` to read the next older page.

## DM progress follows

A follow makes a DM show an issue's progress cards. Members pass
`--chat <chat-session-id>` for a DM they created. Agents pass no `--chat`: the
lane comes from the DM that started the task, and a task with no DM is refused.

- `multica issue follow <issue-id>` starts following.
- `multica issue unfollow <issue-id>` stops following.
- `multica issue mute-follow <issue-id>` mutes the cards, keeping the follow.
- `multica issue unmute-follow <issue-id>` turns the cards back on.
- `multica issue follows --chat <chat-session-id>` lists the follows in a DM.
