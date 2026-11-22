# Issue handoff operation key

A status move can carry an `operation_key`. If the response is lost, the caller
sends the same request again and gets the first result back. The retry writes
nothing: no second comment, status-history row or agent run.

## API reference

`PUT /api/issues/{id}` accepts one extra field next to `status`,
`handoff_comment`, `handoff_assignee` and the other handoff fields.

| Field | Type | Meaning |
| --- | --- | --- |
| `operation_key` | UUID string, optional | Names this move. A move without a key behaves as before and writes no receipt. |

The server writes one receipt row (`issue_handoff_operation`) first in the
status transaction. This holds for every handler type: agent, squad, member
and none. For an agent or squad target the receipt also stores the `dispatch_id`
of the write-ahead dispatch.

| Retry | Result |
| --- | --- |
| Same key, same request body, same actor | `200` with the stored first response. Nothing is written. |
| Same key, different request body | `409` with `code: operation_key_reused`. Nothing is written. |
| Same key, different actor | `409` with `code: operation_key_reused`. Nothing is written. |
| `operation_key` that is not a UUID | `400`. |

The request body is compared on the handoff fields: `status`,
`handoff_assignee`, `keep_for_step`, `handoff_comment`, `handoff_comment_id`,
`attach_comment_ids` and `handoff_preview`.

## 7-day limit

Receipts are kept for 7 days. The handoff sweep deletes every receipt older than
7 days. After the delete, the same key is new: a retry writes a new move.
Do not rely on a key to deduplicate a retry that arrives more than 7 days
after the first request.

## CLI

The CLI does not send `operation_key` yet. The CLI flag and its help text,
including the 7-day limit, ship with the CLI slice (S6 of CHE-2299).
