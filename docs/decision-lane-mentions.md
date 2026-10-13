# Decision DM selection

An issue task opens a decision-intent window with a comment naming the human.
Within that window, `multica chat sessions --issue <issue> --for <person-id>`
returns at most 10 active web DMs between that person and the calling agent.
Listing does not create a DM or consume the window.

Each lane has `id`, `title`, `updated_at` and an `issue_mentions` array.
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

Send the ask to an explicitly selected lane:

```sh
multica decision request --issue <issue> --session <listed-dm-id> --class reversible --ask-file ./ask.md
```

If no listed lane suits, explicitly request a new DM:

```sh
multica decision request --issue <issue> --new-dm --for <person-id> --class reversible --ask-file ./ask.md
```

The API equivalents use `session_id`, or `new_dm: true` with `for`.
Omitting a choice, combining choices or creating a DM without a named person
is rejected. The chosen existing DM must remain in the scoped listing.
An authorized new-DM ping creates `Decisions: <agent name>` in the same
transaction as the request. Refusals roll back creation. Existing window,
membership, authorization and one-ping-per-person checks still apply.

CHE-1610 owns removal of the older summary API, reminder, UI and storage.
Production deployment and migration remain separate human gates.
Jev ranking is a later phase and is not part of this layer.
