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

## Recording the answer

When the person answers in a DM, record it on the issue:

```bash
multica issue comment add <issue> --decision-record \
  --message <dm-message-id> [--context-message <offer-message-id>] \
  [--excerpt "<text>"]... [--content-file ./note.md]
```

- `--message` is the attested DM message. The server quotes it; you cannot
  type the quote. `--content-file` is an optional note, kept apart from the
  quote and collapsed in the UI.
- `--context-message` is the earlier assistant message that offered the
  options. The record shows the matching option label, such as `D1`.
- `--excerpt` quotes only that part of the message. It is repeatable. The
  server refuses text that is not a verbatim part of the message, after
  whitespace is collapsed. Without `--excerpt`, the whole message is quoted.
- When one message answers several asks, record each ask on its own issue and
  pass the excerpt for that record. Do not quote the whole message on every
  issue.
- The full message stays behind a collapsed "Full message" disclosure on an
  excerpted record.

## Record states

A record is pending until the agent is woken, then effective, or disclaimed
by the person. `reversible` records wake at once. Every other class waits 10
minutes. The person can disclaim while it is pending and after it is
effective; an effective record sends the agent a retraction. A disclaim
reopens a live asked request, and cancels the request of an unprompted record
or an archived DM. Times shown to people
are ICT. User-facing description: `docs/decision-records.md` in the repo.

## Decision-bound writes (agent changes a governed field)

An agent may change a workflow's steps or a governed instruction field only
when a workspace owner approved that exact change. The workspace must allow
the agent and the field (the delegated governed writes setting); without it the write is
refused.

1. Author the change. Run the write command with `--print-hash`: it prints
   `before_sha256` and `after_sha256` and writes nothing.
2. Get a PASS from a reviewer of another lab on `after_sha256`.
3. Post the decision request. `decision` is
   `Apply this change to <field>?` then the two lines `before_sha256: <hex>` and
   `after_sha256: <hex>`; `<field>` is `workflow.steps`, `workflow.instructions`,
   `workspace.context`, `squad.instructions` or `agent.instructions`. `options`
   must include a card labelled exactly `Approve write`; the owner answers with
   that label alone (a note makes it conditional and the server refuses it).
4. After the owner approves, repeat the step 1 command with `--decision-id
   <decision-id>` instead of `--print-hash`. The decision is single use and
   expires after 7 days.
5. Read the value back and compare its hash with `after_sha256`.

```bash
multica workflow set-instructions <id> --status in_progress --file ./step.md --print-hash
multica workflow update <id> --steps-file ./steps.json --decision-id <decision-id>
multica agent update <id> --instructions-file ./i.md --expected-before-digest <hex> --decision-id <decision-id>
multica squad update <id> --instructions-file ./i.md --expected-before-digest <hex> --decision-id <decision-id>
multica workspace update <id> --context-file ./c.md --expected-before-digest <hex> --decision-id <decision-id>
```

- Instruction fields hash the raw UTF-8 bytes; pass the live value's hash as
  `--expected-before-digest`. The CLI computes `after_sha256` from the file.
- Workflow commands hash the live workflow read just before the write (steps as
  canonical JSON wrapped with the workflow id), so no `--expected-before-digest`.
  `after_sha256` is the hash of the steps as the server stores them: the CLI
  applies the server's own trim, handler-default and board-order rules (and adds
  missing `todo`, `done` and `cancelled` steps), so a newline-terminated
  instruction file or a partial `--steps-file` hashes like the stored result.
- A bound write carries one field: no other update flag, and for workflows no
  run-rule flag. `--decision-id` and `--vote-issue` are alternatives.
- 409 means the live value moved or the decision was used: read it again and
  ask for a new decision. The CLI never retries.
