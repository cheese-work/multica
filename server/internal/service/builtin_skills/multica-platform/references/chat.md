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

## DM triage

```bash
multica chat attention [--stale] --output json   # DMs that wait for you, oldest first; --stale = yellow (4h) or red (24h) only
multica chat later <session> 2h                  # snooze a DM: 90m, 2h, 1d12h, seconds or an RFC3339 time (1s..168h)
```

Retry `chat later` with the same `--request-id <uuid>` so the retry does not
extend the time.
