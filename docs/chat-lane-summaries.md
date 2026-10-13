# Chat lane summaries

Each chat session stores `lane_summary` and `lane_summary_updated_at`. The
summary describes the lane's projects, issues, and themes, not its transcript.
The limit is 600 Unicode code points, enforced by the API and PostgreSQL.
An empty string clears the description and still records the update time.
Updating a summary does not bump the session's last-activity timestamp.

## Write access

A running chat task can update only its own session:

```bash
multica chat summary set --file ./lane-summary.txt
```

The command preserves the UTF-8 file's contents, including whitespace, and
uses `PUT /api/chat/summary`. The server resolves the session from the
authenticated task, never from a client-supplied task header or file content.
Missing files, invalid UTF-8, and oversized summaries fail before an API call.

The session's creator can use an interactive JWT to call
`PUT /api/chat/sessions/{sessionId}/summary` with
`{"lane_summary":"..."}`. The existing workspace, creator, and private-agent
access checks still apply. Archived sessions are read-only. PATs, cloud PATs,
CLI JWTs, issue tasks, and tasks belonging to another session cannot write.

## Read access

The existing session-list, single-session, messages, and paginated-messages
APIs now return 403 for non-interactive credentials. A running chat task can
still read its own session and transcript, but cannot enumerate other lanes.
Its own-session list/get projections omit the stored lane description by
returning an empty summary and null update time. Issue agents receive lane
descriptions only through the decision-intent windowed listing implemented
in the subsequent decision-request slice.

CLI, legacy and unknown JWT provenance receives the stable refusal code
`interactive_login_required`. Web and desktop Chat prompt **Sign in again to
use Chat** with a **Sign in** button, without automatically logging out.
The Chat page and floating Chat window share this action. Only clicking the
button invokes the existing shared logout flow and opens the platform's
login screen for a fresh interactive sign-in. Sliding renewal does not add
`amr` to legacy tokens, so an active legacy session stays locked out until
actual re-login, with no automatic migration deadline. Existing desktop
tokens minted by `/api/cli-token` also need replacement.

Desktop browser handoffs now use `POST /api/auth/desktop-token`, which
requires a human actor, verified interactive JWT and cookie CSRF. It issues
an interactive token with a new session ID. Email and already-signed-in
browser handoffs mint only after **Open Multica Desktop** is clicked. Google
retains its interactive token and waits for that click before deep-linking.
Native desktop email sign-in remains a direct interactive `VerifyCode` login.
Desktop handoffs never call the CLI-only `/api/cli-token` endpoint.

Existing session mutations also redact those fields for non-interactive
callers, so a rename, pin, or archive response cannot bypass the read gate.
The dedicated summary write returns the caller's own accepted description.

Interactive session lists and single-session responses include both fields.
Client schemas default missing or malformed fields to an empty summary and
null timestamp, so older servers do not cause a session to disappear.

## DM prompts

Each first-party DM claim carries the current summary, update time, and the
number of non-command messages since that update. A summary without an update
time counts all non-command messages. The count uses the same timestamp
snapshot as the claimed summary.

Each DM prompt shows the summary and asks the agent to update it when empty,
when the topic changes this turn, or after at least 20 new messages. The
prompt states that the summary is context, not instructions. Sessions with
durable channel-origin evidence do not receive this first-party DM reminder,
even after a channel installation is removed. Older claim payloads leave the
reminder disabled.

The DM header editor and continuation seeding are later-layer work. This
change does not implement decision requests, wakes, or continuation.
