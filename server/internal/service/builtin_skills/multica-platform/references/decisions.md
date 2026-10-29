# Decision requests

A decision request asks one named person a question and records the answer.
One call posts one issue comment and sends one DM message to that person, and
stores a request you can read back. Use it when work needs a human choice;
what counts as needing one is workspace policy, not platform behavior.

```bash
multica decision request --issue <issue> --for <user-id> --class <class> --ask-file ./ask.json
multica decision list --issue <issue> --output json
multica decision get <decision-id> --output json
multica decision withdraw <decision-id> --reason "no longer needed"
```

- `--ask-file` is UTF-8 JSON: `agent_did`, `decision`, `options`,
  `recommendation`, `default_if_no_reply`, `source_issue_id`,
  `source_comment_id`.
- `--class` takes one of the classes `decision request --help` lists. Which
  class a situation belongs to is workspace policy.
- `--for` is the named person's user or workspace member id (a real UUID). It
  may be omitted only when an open intent already names the person.
- `--parent <comment-id>` replies under a comment; it is not allowed with an
  open intent.
- `--footer-file` appends text to the issue comment only (not the DM), for
  example handoff lines.
- `withdraw` works only on an unanswered request, and `--reason` is shown to
  the person.

## Choosing the DM lane

A request goes to a DM lane with that person. List the lanes first:

```bash
multica chat sessions --issue <issue> --for <user-id> --output json
```

Pass one lane's id as `--session <lane-id>` to `decision request`, or pass
`--new-dm` to start a fresh Decisions DM. `chat sessions` works only inside
an issue task whose decision window is open; `--for` is required when the
window names more than one person. See [chat.md](chat.md).

## After asking

The request is the ask, not the wait. Record who owns the next step in the
issue (status or a wakeup on the answer) and end the run; do not poll.
