## Candidate

Approve a workspace configuration change with a passkey on a decision card (CHE-3348 step 10). The viewer applies the locked card design `design/you-are-approving-card/approved/approving-card-b.html` @ `8ed2e49da2e4fd7c7aeb18f780165ebcd80fd1ad`.

## Harness

`harness/` is a small Vite page that mounts the real `DecisionRequestCard`, the real config-change viewer and the real approval flow. It stubs only three things: the passkey ceremony, the member role (`?role=`) and the current workspace. Server calls return fixtures; `?refuse=<code>` makes the approve call fail with that code. It is fixture evidence for the front end, not a server acceptance run.

## Reproduce On X99

Run from the repository root after `pnpm install --frozen-lockfile`:

```bash
cd apps/desktop && ../../node_modules/.bin/vite build --config ../../docs/ux/CHE-3483/harness/vite.config.ts && cd ../..
node docs/ux/CHE-3483/capture.mjs
```

The script needs `/usr/bin/google-chrome`. Output goes to `/tmp/che3483-captures/` (`CAPTURE_OUTPUT` overrides it). The PNGs in `captures/` come from this run at 1440x900 and 390x844.

## Before

The surface is new. Before this change a card for these action types had no server preview, so it showed "Details unavailable" with Approve disabled. `captures/owner-details-unavailable-*.png` shows that same fail-closed frame.

## Captures

| Frame | State |
| --- | --- |
| `owner-context-closed` | owner, viewer collapsed, Approve enabled |
| `owner-context-changes` | owner, Changes tab open, text diff |
| `owner-instructions-changes` | owner, two hunks with hidden unchanged lines |
| `owner-settings-result` | owner, settings keys and Result tab |
| `member-context-no-approve` | member: owner note, no Approve, Reject stays |
| `owner-refused-payload-mismatch` | refusal code shown, Approve stays for retry |
| `owner-refused-class-needs-owner` | final refusal, Approve removed |
| `owner-details-unavailable` | field outside the allowlist, Approve disabled |
