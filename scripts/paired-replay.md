# Paired replay instrumentation

This tool verifies the qualified historical input, writes a metadata-only pair plan, and measures two completed local Codex session logs. It does not launch Codex, call a model, access Multica, or mutate a session. Run model-backed replay only after the separate spend gate is satisfied.

Create a plan from the read-only historical fixture and the same local Verification Invariant readbacks that both options will use:

```sh
node scripts/paired-replay.mjs plan \
  --fixture "$HISTORICAL_FIXTURE" \
  --readbacks "$VERIFICATION_READBACKS" \
  --threshold 80000 \
  --output /tmp/che-1942-pair-plan.json
```

The plan fails if the fixture, prefix, prompt, session metadata, or model differs from the qualified source. It stores only SHA-256 digests for the fixture prefix and readbacks. It identifies `fresh-session-reconstruction` and `resume-and-compact` as the paired options and records the same fixture, readbacks, model, runtime, and configurable threshold.

After an independently authorized execution produces one isolated session log per option, collect response-linked usage:

```sh
node scripts/paired-replay.mjs measure \
  --plan /tmp/che-1942-pair-plan.json \
  --fresh-log "$FRESH_SESSION_LOG" \
  --fresh-session-id "$FRESH_SESSION_ID" \
  --fresh-root-turn-id "$FRESH_ROOT_TURN_ID" \
  --fresh-start-ordinal -1 \
  --fresh-model gpt-6.1-sol \
  --fresh-runtime-id bc0835c7-a25d-4a16-a3bc-e2921d092ed9 \
  --resume-log "$RESUME_SESSION_LOG" \
  --resume-session-id "$RESUME_SESSION_ID" \
  --resume-root-turn-id "$RESUME_ROOT_TURN_ID" \
  --resume-start-ordinal 1544 \
  --resume-model gpt-6.1-sol \
  --resume-runtime-id bc0835c7-a25d-4a16-a3bc-e2921d092ed9 \
  --output /tmp/che-1942-pair-report.json
```

Measurement fails closed when a response record is missing, duplicated, unbound, malformed, or inconsistent with the before/after `token_count` snapshot delta. It uses the latest snapshots only as a completeness check; it never sums heartbeat events or reports their totals as after-values. The JSON report contains per-response `input_tokens` and `cached_input_tokens`, per-option aggregates, maximum per-call context, and fixture/model/runtime/threshold provenance. It does not accept CHE-1931.

`assertReplayActionAllowed` permits local-sandbox reads/writes and platform or production-session reads. It denies platform writes and production-session mutations. The CLI has no executor or mutation interface; `plan` and `measure` remain offline-only.
