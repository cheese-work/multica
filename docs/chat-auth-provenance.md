# Chat Auth Provenance

Chat message roles describe conversation structure, not authorship. A task token can carry the runtime owner's user ID, so `role=user` alone cannot establish that the human wrote a message.

## Authentication Paths

The auth middleware strips client-supplied `X-Actor-Source`, `X-Agent-ID` and `X-Task-ID` headers before resolving credentials. It stamps `X-Actor-Source` from the verified credential:

| Credential | Server auth-path stamp |
| --- | --- |
| JWT issued by email verification or Google login, with `amr=interactive` | `interactive_jwt` |
| CLI handoff JWT, with `amr=cli` | `cli_jwt` |
| Legacy JWT without `amr`, or an unrecognized `amr` | `cli_jwt` |
| Personal access token, including a cache hit | `pat` |
| Agent task token | `task_token` |
| Cloud node PAT | `cloud_pat` |

Cookie and bearer transport use the same claim classification. Moving a CLI token into an auth cookie does not make it interactive. Existing cookie CSRF checks still apply.

`POST /api/cli-token` requires a human actor **and** an interactive JWT. It returns a token with `amr=cli`. PATs, task tokens, cloud PATs, CLI JWTs and legacy JWTs cannot use this endpoint to mint an interactive token or a new CLI handoff token. A legacy browser session needs a fresh interactive sign-in before obtaining a CLI handoff token.

## Message Insertion

Migration `500_chat_message_auth_provenance` adds nullable `chat_message.sent_via`. Existing messages remain NULL; there is no authorship backfill. The database limits non-NULL values to `interactive_jwt`, `cli_jwt`, `pat`, `task_token`, `cloud_pat`, `channel` and `system`.

Direct-chat sends carry only the middleware stamp into the transaction that creates the task and its input message. Client JSON fields do not provide provenance. Channel-ingested messages are stamped `channel` by the shared insert query. Other automated messages, including kickoff and assistant messages, default to `system`.

The stamp is a necessary input to later decision-answer authorship checks, not a complete attestation by itself. Human, workspace, session, task and time-window checks remain separate. This slice does not implement decision requests, durable channel origin or lane summaries.

## Verification

The regression tests cover both interactive login issuers, bearer and cookie JWT classification, forged headers and JSON fields, every machine credential, a real PAT-cache hit on isolated Redis, CLI remint rejection, automated messages, historical NULL preservation and migration rollback. DB-backed tests use the checkout's isolated test database and `REDIS_TEST_URL` for the isolated Redis fixture.
