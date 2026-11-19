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

## Come back to this DM later

```bash
multica chat wakeup create --title "CI on PR 12" --until-pr cheese-work/multica#12:checks --instruction "report the result" --output json
multica chat wakeup create --title "review reply" --until-issue CHE-1 --state in_review --instruction "relay it" --output json
multica chat wakeup create --title "deploy check" --after 25m --instruction "check the deploy and report" --output json
multica chat wakeup create --title "nightly build" --every 30m --expires-in 48h --instruction "check the build" --output json
multica chat wakeup list --output json            # this DM's wakeups
multica chat wakeup get <wakeup-id>
multica chat wakeup update <wakeup-id> ...        # replaces the configuration; deadlines restart from now
multica chat wakeup cancel <wakeup-id>            # the user no longer needs it
multica chat wakeup finish <wakeup-id>            # end a --every check: what it watched for happened
multica chat wakeup checkin <wakeup-id> --note "CI still running"   # end a check turn quietly
```

- Create one when you act toward someone else for the user and they want the
  outcome back, and whenever you tell the user you will report back.
- `--title` (what you wait for, in the user's words) and `--instruction` (or
  `--instruction-file`) are required. Pick exactly one kind:
  - `--until-issue ISSUE` (`--state done|ended|in_review|blocked`, or `--status`),
    `--until-children-done ISSUE` (`--stage N`), `--until-pr owner/repo#N[:merged|closed|checks]`,
    `--until-run TASK_ID`, `--until-reply ISSUE` (`--from <user-id>`): the platform
    checks the fact and wakes you once it holds.
  - `--at <RFC3339>` or `--after <duration>`: a single reminder.
  - `--every <interval>` (at least 10m): a periodic check; quiet check-ins send nothing.
- Every wakeup ends: it runs until `--expires-in` / `--expires-at` (default 24h,
  at most 168h), then wakes you once more to say where things stand. A DM holds
  at most 10 active wakeups.
- The wakeup starts a new turn in this DM later (and its IM thread, when it came
  from one); finish the current turn instead of waiting for it.
- Recurring work (every morning, every Friday) is an Autopilot, not a chat wakeup.
- To start an agent run on an issue, use `multica issue wakeup` (see [wakeups.md](wakeups.md)).

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
