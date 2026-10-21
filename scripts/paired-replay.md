# Paired replay instrumentation

This tool verifies the qualified historical input, writes a metadata-only pair plan, and measures two completed local Codex session logs. It does not launch Codex, call a model, access Multica, or mutate a session. Run model-backed replay only after the separate spend gate is satisfied.

The qualified source can grow after qualification. Freeze the qualified byte range into a private copy; do not truncate or otherwise modify the live source. For this sample, the qualified copy is 12,689,577 bytes. Check its pinned digest before planning. Stop if the digest does not match; do not update the qualified digest to accept changed source bytes.

```sh
umask 077
set -o noclobber
head -c 12689577 "$HISTORICAL_FIXTURE" > "$FROZEN_FIXTURE"
test "$(wc -c < "$FROZEN_FIXTURE" | tr -d '[:space:]')" = 12689577
printf '2ec89b207ebc469831ea7e56be41145048d23b2123bcf175272c8d235815efff  %s\n' "$FROZEN_FIXTURE" | sha256sum --check
chmod a-w "$FROZEN_FIXTURE"
```

Create a plan from that frozen copy and the same local Verification Invariant readbacks that both options will use:

```sh
node scripts/paired-replay.mjs plan \
  --fixture "$FROZEN_FIXTURE" \
  --readbacks "$VERIFICATION_READBACKS" \
  --threshold 80000 \
  --output /tmp/che-1942-pair-plan.json
```

The plan fails if the frozen fixture, prefix, prompt, session metadata, or model differs from the qualified source. It stores SHA-256 digests for the complete fixture, input prefix, readbacks, and plan contents. It identifies `fresh-session-reconstruction` and `resume-and-compact` as the paired options and records the same fixture, readbacks, model, runtime, and configurable threshold.

After an independently authorized execution produces one isolated session log per option, collect response-linked usage:

```sh
node scripts/paired-replay.mjs measure \
  --plan /tmp/che-1942-pair-plan.json \
  --fresh-log "$FRESH_SESSION_LOG" \
  --fresh-session-id "$FRESH_SESSION_ID" \
  --fresh-root-turn-id "$FRESH_ROOT_TURN_ID" \
  --fresh-start-ordinal -1 \
  --resume-log "$RESUME_SESSION_LOG" \
  --resume-session-id "$RESUME_SESSION_ID" \
  --resume-root-turn-id "$RESUME_ROOT_TURN_ID" \
  --resume-start-ordinal 1544 \
  --output /tmp/che-1942-pair-report.json
```

Measurement validates the plan digest and the qualified fixture and prefix digests. The fresh and resume logs must use distinct session IDs. The tool reads runtime, CLI version, and history mode from each log's `session_meta`; it reads model identity from `turn_context`. Missing or mismatched log provenance fails closed. Operator flags do not supply runtime or model provenance.

Measurement also fails closed when a response record is missing, duplicated, unbound, malformed, or inconsistent with the before/after `token_count` snapshot delta. It uses the latest snapshots only as a completeness check; it never sums heartbeat events or reports their totals as after-values. A decrease in any cumulative `token_count` field fails closed because reset semantics are not qualified here. Real compaction-reset semantics remain `NOT-RUN`. The JSON report contains per-response `input_tokens` and `cached_input_tokens`, per-option aggregates, maximum per-call context, and fixture/model/runtime/threshold provenance. It does not accept CHE-1931.

`assertReplayActionAllowed` permits local-sandbox reads/writes and platform or production-session reads. It denies platform writes and production-session mutations. The CLI has no executor or mutation interface; `plan` and `measure` remain offline-only.
