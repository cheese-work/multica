## Problem

<!-- What is the concrete problem, who is affected, and why does it matter? -->

## Goal

<!-- What should be true after this change? State the intended outcome. -->

## Proposal

<!-- Explain the high-level design and why it achieves the goal. Make it understandable to a reader without much project background. Keep implementation details to a minimum. -->

## Alternatives (optional)

<!-- If useful, briefly describe other approaches and why you did not choose them. -->

## Screenshots

<!-- Required for UI or interaction changes: attach screenshots or a screen recording from the running app showing the affected screens and states. Without either, these PRs are rejected by default. Before/after comparisons are strongly preferred; for a new screen, include the after screenshot. Upload with scripts/pr-shots (see CONTRIBUTING.md#screenshots-and-recordings) or GitHub's web editor. If there is no UI or interaction impact, write "N/A" and explain why. -->

<!-- Link the issue this PR addresses. If no issue exists, consider creating one first.
     Replace the line below with exactly one disposition, using the issue's full
     key (e.g. CHE-123), not a bare `#number`:
     - Full delivery of the issue's acceptance criteria: Closes CHE-123
     - Partial delivery, a stack layer, or a broader parent: Refs CHE-123, plus a
       note on the remaining scope and which PR is intended to actually close it.
     This section must contain no other line starting with Closes/Fixes/Resolves/Refs. -->

REPLACE-THIS-LINE-WITH-YOUR-DISPOSITION

## Changelog

<!-- Replace the line below with one sentence a user notices, in English, plain text,
     at most 140 characters (a longer line fails the PR check). It publishes as this
     PR's entry on /changelog. Keep the line verbatim, like Closes/Refs. A PR
     without a valid line is hidden from /changelog; the placeholder below counts
     as missing.
     The section follows the title type (feat, fix, perf); override it with
     Changelog(feat|fix|perf): <sentence>.
     Good:
       Changelog: Issue lists now open instantly in large workspaces.
       Changelog: none
       Changelog(fix): Comments that end in an emoji no longer lose their last word.
     Bad (plumbing, not user-facing):
       Changelog: Refactor X store
       Changelog: CHE-123 add Y
     In a stack, only the PR that lands on `main` carries the line, for the whole stack.
     `Changelog: none` is only for internal-only changes (CI, scripts, docs, tests). A
     feat, fix or perf PR that changes user-facing code fails the check with `none`:
     write the sentence, or use a non-user-facing type (ci, test, chore).
     Upstream-sync PRs use `Changelog: none`. -->

Changelog: <one user-facing sentence, or none>

## Type of Change

- [ ] Bug fix (non-breaking change that fixes an issue)
- [ ] New feature (non-breaking change that adds functionality)
- [ ] Refactor / code improvement (no behavior change)
- [ ] Documentation update
- [ ] Tests (adding or improving test coverage)
- [ ] CI / infrastructure

## Changes Made

<!-- List the specific changes. Include file paths for code changes. -->

-

## How to Test

<!-- Steps to verify this change works. For bugs: reproduction steps + proof that the fix works. -->

1.
2.
3.

## Cost delta

<!-- Required (CHE-4396). One line each; numbers or a measured range, `unknown` only with how it will be measured. -->

- C1 Cost delta: <vs today, per day or per run: tokens (which model), agent runs, Actions runs/minutes, API calls, USD>
- C2 Optimality check: <cheaper or simpler alternative considered, why rejected>
- C3 Watch: <how observed after rollout (daily usage report, CHE-4324); rollback threshold>

## Checklist

- [ ] I have included a thinking path that traces from project context to this change
- [ ] I have run tests locally and they pass
- [ ] I have added or updated tests where applicable
- [ ] If this change affects the UI, I have included before/after screenshots
- [ ] Docs updated (page path under `apps/docs/content/docs/`, all locales) / not user-facing (reason): <path or reason>
- [ ] If I added a new runtime / coding tool / UI tab, I synced the change to **landing copy** (`apps/web/features/landing/i18n/`) and **relevant docs** (`apps/docs/content/docs/`)
- [ ] If this PR touches Chinese product copy, I checked it against `apps/docs/content/docs/developers/conventions.zh.mdx` (terminology, mixed-rule for `task` / `issue` / `skill`)
- [ ] I have considered and documented any risks above
- [ ] I will address all reviewer comments before requesting merge

## AI Disclosure

<!-- Most PRs involve AI coding tools — that's totally fine! We're curious about your process. -->

**AI tool used:** <!-- e.g. Claude Code, Cursor, GitHub Copilot, Multica Agent, N/A -->

**Prompt / approach:**
<!-- How did you use AI to produce this code? Share your prompt, conversation link, or describe your approach. This helps the team learn from each other's AI workflows. -->
