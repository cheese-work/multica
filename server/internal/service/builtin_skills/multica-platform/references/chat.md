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

## Rename a DM

```bash
multica chat rename <session-id> <title> --output json
```

Sets the DM title and posts a message announcing the new title and who
changed it, without reading the DM's contents. JSON carries `id`, `title`,
`previous_title` and `message_id`.

## Schedule a wakeup in this DM

```bash
multica chat wakeup create --after 25m --instruction "check the deploy and report" --output json
multica chat wakeup create --at 2026-11-01T09:00:00+07:00 --instruction "..." --output json
multica chat wakeup list --output json                  # pending wakeups of the current DM
multica chat wakeup cancel <wakeup-id>
```

- Give exactly one of `--after <duration>` (`25m`, `2h`) or `--at <RFC3339>`.
  `--instruction` is required: it is what the scheduled turn does when it fires.
- The wakeup starts a new turn in this DM later; finish the current turn
  instead of waiting for it.
- These are DM wakeups. Issue wakeups use `multica issue wakeup`
  (see [wakeups.md](wakeups.md)).

## DM lanes for decisions

`multica chat sessions --issue <issue> --for <user-id>` lists the DM lanes a
decision request can use. See [decisions.md](decisions.md).
