# Decision DM selection

An issue task opens a decision-intent window with a comment naming the human.
Within that window, `multica chat sessions --issue <issue> --for <person-id>`
returns at most 10 active web DMs between that person and the calling agent.
Listing does not create a DM or consume the window.

Each lane has `id`, `title`, `updated_at`, `eligible` and an `issue_mentions` array.
Lanes are ordered by relevance to the selected issue: the DM that named it most
recently first, then DMs that never named it. `eligible` is true only when a
message or decision request in the DM names the selected issue.
Each mention has `issue_id`, `issue_key`, `issue_title`, `mentioned_at`,
`relative_time` and `snippet`. Timestamps are RFC3339 machine values.
The array contains at most 20 distinct same-workspace issues, newest first.
The snippet contains at most 100 Unicode characters surrounding the mention.
Markdown links render as visible text before truncation. The selected issue
link and repeated issue key/title text are omitted from the context snippet.
Whitespace is collapsed and mention URLs are removed.
An empty lane still appears with `issue_mentions: []`.

Only ordinary user and assistant messages count. Continuation summaries,
continuation notices, failed assistant output and decision-request pings do not
count. Missing issues and foreign-workspace issues do not appear. Mentions are
computed on read using existing message/session indexes; no agent-maintained
summary or write-time mention table is required.

One call needs no window. `multica decision request --issue <issue> --for <person-id>
--class reversible --ask-file ./ask.json [--footer-file ./footer.md]` runs the
window checks inline for the named person, then writes one issue comment, one
DM message and one ping. The server picks the lane: the person's lane with this
agent that holds the latest request on this issue, else the newest listed lane
that mentions this issue, else a new `Decisions: <agent name>` DM. Briefs and
channel DMs are never listed lanes.

A request goes only to a DM where that issue was discussed. `--session` is
refused unless a message or decision request in that DM names the issue; the
error lists the matching DMs. The check reads that DM, so an older discussion
stays eligible however many newer DMs exist. `--new-dm` opens a DM for this
issue only, and a request for another issue is refused there until that issue is
discussed in it. The same open decision (same issue, agent and person, ask
compared without its source link) is refused for 24 h after the earlier one; the
error carries the earlier ask time.

With an open decision-intent comment the two-step flow below is unchanged.

Send the ask to an explicitly selected lane:

```sh
multica decision request --issue <issue> --session <listed-dm-id> --class reversible --ask-file ./ask.json
```

If no listed lane suits, explicitly request a new DM:

```sh
multica decision request --issue <issue> --new-dm --for <person-id> --class reversible --ask-file ./ask.json
```

The API equivalents use `session_id`, or `new_dm: true` with `for`. The
one-call form sends `for` alone; `footer` and `parent_id` are optional.
Combining `session_id` with `new_dm`, creating a DM without a named person,
or sending none of `session_id`, `new_dm` and `for` is rejected. The chosen existing DM must remain in the scoped listing.
An authorized new-DM ping creates `Decisions: <agent name>` in the same
transaction as the request. Refusals roll back creation. Existing window,
membership, authorization and one-ping-per-person checks still apply.

## Structured ask

`--ask-file` takes a UTF-8 JSON object, not free-form Markdown:

```json
{
  "context_eli10": "We can ship the release today or tomorrow. The code is tested. Shipping during peak hours could slow users down. You pick the day.",
  "agent_did": "Ran the scoped tests and published the candidate.",
  "decision": "Choose the deployment window.",
  "stakes_if_wrong": "A bad window can slow the service for users.",
  "options": [
    {
      "label": "DEPLOY NOW",
      "what_happens": "Agents deploy the release this hour.",
      "pros": ["Users get the fix today"],
      "cons": ["Peak-hour traffic may slow the service"],
      "cost_or_time": "No extra time"
    },
    {
      "label": "DEPLOY TOMORROW",
      "what_happens": "Agents deploy after the maintenance window opens.",
      "pros": ["Quiet hours, lower risk"],
      "cons": ["The fix waits one day"],
      "cost_or_time": "One day of delay"
    }
  ],
  "recommendation": "DEPLOY TOMORROW after the maintenance window opens.",
  "default_if_no_reply": "Do not deploy. Keep the current version running.",
  "source_issue_id": "11111111-1111-4111-8111-111111111111",
  "source_comment_id": "22222222-2222-4222-8222-222222222222"
}
```

All fields are required. `context_eli10` is two to five plain sentences:
what this is for the reader, and why it needs a decision now. Options are
objects with a nonblank `label`, `what_happens` and `cost_or_time` (plain text
such as "No extra time" is fine; do not invent a number) and at least one
`pros` and one `cons` item. Use at least two options with distinct labels.
Unknown keys and legacy string options are rejected with an error that names
the field. The source issue must match `--issue`. The source comment must exist
on that issue in the selected workspace and must not be deleted. The API
accepts the same object as `ask` in `POST /api/decisions`.

An ask that approves a file adds two optional fields. `subject` is
`artifact_approval`; it requires at least one entry in `artifacts[]`. Each
artifact has `attachment_id`, `label`, `sha256` (64 lowercase hex characters),
and optionally `supersedes_attachment_id`, `signed_by` and
`supersedes_signed_by` (each `reviewer` and the signed review `comment_id`;
`supersedes_signed_by` needs `supersedes_attachment_id` and names who signed
the file the diff is measured against). The server refuses the request when an attachment
is not on the source issue in the workspace, or when the stored bytes do not
hash to `sha256`. Both signatures are recorded as given and are not checked against
the comment. `GET /api/decisions/{id}/artifacts` returns each artifact with its
file card and, when it supersedes an earlier text attachment, the changed
hunks only (`diff.hunks`, lines prefixed ` `, `-` or `+`). `diff.problem` is
`not_text`, `too_large` or `unavailable` when no hunks can be shown.

Advisory lint: the CLI prints a `hint:` line on stderr, and the API returns
`hints[]`, when `context_eli10`, `stakes_if_wrong` or an option's
`what_happens` holds a commit SHA or an internal code such as `L2` or `RGS-1`,
or when `context_eli10` is not two to five sentences. Hints never block a
request.

The server renders the fields and source links into the stored ask, issue
comment and DM message. The stored request also retains `source_comment_id`
and `source_content_snapshot`. Editing the source comment does not rewrite
that snapshot. Invalid requests leave no ask, DM or notification behind.
The composed issue comment, including the optional footer, must fit the
normal comment size limit.

An identical structured request from the same running task returns the
original request with HTTP 200. The first creation returns HTTP 201. A retry
does not create another comment, DM or ping, or reopen a terminal request.

## Withdraw an unanswered ask

```sh
multica decision withdraw <request-id> --reason "Superseded by the newer candidate."
```

`POST /api/decisions/<request-id>/withdraw` takes `{"reason":"..."}`. The
reason must be nonblank and at most 2000 bytes. The named human can withdraw
an ask through authenticated human credentials. The asking agent can withdraw
it from a running task on that issue or in that decision's DM.

The request becomes `cancelled` with `cancel_reason: "agent_withdrawn"`,
`withdraw_reason`, `withdrawn_at`, and the withdrawing task or user identity.
The original ask and source snapshot remain unchanged. One cancellation
comment is created. An active DM receives one withdrawal reference. A retry
with the same reason returns the original result without duplicate writes.
Another terminal state or a changed reason returns HTTP 409.

Answer recording and withdrawal lock the same request. Exactly one can move
an open ask to a terminal state. Withdrawal cannot revoke an existing answer;
the separate decision-answer disclaim operation handles that case.

## Record an answer given elsewhere

```sh
multica issue comment add <issue> --decision-record --parent <ask-comment-id> --message <human-message-id>
multica issue comment add <issue> --decision-record --parent <ask-comment-id> --source-comment <human-comment-id>
```

A running DM task or an assigned running task on the issue can record an
answer to an existing ask. `--message` is repeatable; all messages must belong
to one ordinary same-workspace DM owned by the request's named human. The DM
can differ from the ask's DM, including a DM with another agent. Messages
must be interactive human messages, not channel-ingested or agent messages.

`--source-comment` instead selects one nondeleted member comment by that same
human on any issue in the selected workspace. It requires `--parent`, and
cannot combine with `--message` or `--context-message`. The API field is
`source_comment_id`. The source must follow the ask's ping and precede the
answering task's start. Source IDs and exact quoted text are snapshotted in
the answer; retries return that same answer instead of quoting edited text.
Deleting a source invalidates its answer through the existing source checker.

An unprompted decision still requires a running task in its own DM and an
explicit class. External issue-comment sources do not create unprompted
decisions. Existing hold, turn-completion, disclaimer and delivery gates stay
in force.

CHE-1610 owns removal of the older summary API, reminder, UI and storage.
Production deployment and migration remain separate human gates.
Jev ranking is a later phase and is not part of this layer.
