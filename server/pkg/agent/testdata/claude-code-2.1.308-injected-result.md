# Injected-result fixture provenance

The JSONL fixture is protocol-derived, not a captured live stream. It models
an orphaned task-notification turn finishing before the daemon's prompt.
Both turns use the same session. No result event needs an `origin` field.

The signal was checked against the installed Claude Code 2.1.308 native
binary, SHA-256
`356733f185fa988e6bf3aa9a555269fdd167982a2e2a80d3c8a78037bb92ad58`.
The following byte offsets refer to its embedded JavaScript, read as Latin-1:

| Offset | Relevant producer behavior |
| --- | --- |
| 276918961 | The stream-input user branch enqueues the incoming frame's `q.uuid`. |
| 276404916 | `ET` copies the queued command's UUID into the turn options. |
| 276565366 | A non-meta turn takes the client UUID into `$a` and passes it to `cm`. |
| 276212430 | `cm` builds `user_message_uuid` and the consumed `user_message_uuids` list. |
| 276574549 | The early prompt-check/local-command result uses those attribution fields. |
| 276582307 | The final result builder applies the attribution fields, including error variants. |
| 276529590 | `wR` stamps `result_index`, starting at zero. |
| 276712801 | The headless stream transport installs `wR` as its result-delivery store. |
| 276815370 | The result-emission path receives that store through `ub`. |

The fixture's empty injected result has `result_index: 0` but no client
prompt attribution. The fake reads the daemon's actual prompt UUID and
echoes it in the owned result. Another case lists that UUID among prompts
folded into a turn whose primary UUID differs. Neither result text nor a
successful pipe write establishes ownership.

Results without numbering or either attribution field retain the existing
single-turn handling. This preserves untagged normal, empty and error
responses; it does not claim to distinguish older untagged injected turns.
The regression runs with task supplements enabled and disabled. It checks
that a control reply can still be written after the injected result, and
that stdin closes after the owned result. Cancellation remains covered.

No installed agent CLI or account is accessed by the tests. Live resume
with orphaned background tasks is a separate integration verification.
