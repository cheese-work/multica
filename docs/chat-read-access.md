# Chat read access

The session-list, single-session, messages, and paginated-messages APIs require
interactive credentials. A running chat task can still read its own session
and transcript, but cannot enumerate other lanes. Existing workspace, creator,
and private-agent access checks still apply.

CLI, legacy and unknown JWT provenance receives the stable refusal code
`interactive_login_required`. Web and desktop Chat prompt **Sign in again to
use Chat** with a **Sign in** button, without automatically logging out.
The Chat page and floating Chat window share this action. Only clicking the
button invokes the existing shared logout flow and opens the platform's
login screen for a fresh interactive sign-in. Sliding renewal does not add
`amr` to legacy tokens, so an active legacy session stays locked out until
actual re-login, with no automatic migration deadline. Existing desktop
tokens minted by `/api/cli-token` also need replacement.

Desktop browser handoffs use `POST /api/auth/desktop-token`, which
requires a human actor, verified interactive JWT and cookie CSRF. It issues
an interactive token with a new session ID. Email and already-signed-in
browser handoffs mint only after **Open Multica Desktop** is clicked. Google
retains its interactive token and waits for that click before deep-linking.
Native desktop email sign-in remains a direct interactive `VerifyCode` login.
Desktop handoffs never call the CLI-only `/api/cli-token` endpoint.

## Lane-summary removal

Deploy the code removal before merging the separate migration 615 PR.
The C00 deploy applies migrations before the blue/green cutover. Dropping
the columns in the code-removal deploy would break the old colour while it
still serves traffic. The code-removal PR leaves both database columns in
place and uses `server/pkg/db/schema/retired_chat_session_lane_summary.sql`
only as sqlc's offline schema input. The runtime migration runner reads
`server/migrations/`, not this code-generation schema.

Migration 615 drops `chat_session.lane_summary` and
`lane_summary_updated_at` only after the column-independent server is
deployed to C00. The column drop also removes rollback compatibility with
older servers that select the columns. Before applying the migration to a live database,
read the non-empty row count and report any non-zero count on CHE-1610:

```sql
BEGIN READ ONLY;
SELECT count(*) AS non_empty_lane_summaries
FROM chat_session
WHERE lane_summary <> '';
COMMIT;
```

The down migration restores empty summary columns and their original length
constraint. It does not recover deleted summaries. Historical migration 596
remains unchanged. The CLI command, write API, task-claim fields, session
response fields, and DM-turn reminder are removed. Client schemas accept
legacy summary fields from older servers as unknown data. Chat read authorization and
continuation summaries remain unchanged.
