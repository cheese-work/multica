# CHE-3531: plain Comment link opens a parked issue

## Immutable inputs

- Runner source: `cheese-work/artemis` commit `cbf2fea84cbd47ea54053239679ab521b01d4d23` (Artemis Pro; pinned source only, this case does not claim device-class qualification).
- Runner dependency lock: `cheese-work/artemis/uv.lock` at the runner commit above (Git blob `dc2d35b8c284055c92ff1cd2560c8201e648bf9f`).
- Runner CLI configuration: `cheese-work/artemis/pyproject.toml` at the runner commit above (Git blob `a36b834875b465bf33e221ffcffc003d4ca94ec4`).
- Fixed app source: `cheese-work/multica` commit `b9715570402d6d38e3a8dce12317b5a8724c7329` (PR #918).
- Pre-fix app source: `cheese-work/multica` commit `1a9e20e40d84916e0bca3b1358e77be7de22565b` (parent of the first fix commit).
- Requested target class: X99 AVD `x99_api35_medium_phone`, API 35, x86_64. The executor must use the exact serial and actions granted by the Android device squad on CHE-3531.
- Keep the built APK SHA-256, build command, runner checkout SHA, device serial, testcase Git SHA, and testcase file SHA-256 in the execution report.

## Cases

Use an already authenticated, authorized workspace account. Do not record credentials or unrelated personal data. Do not create, edit, answer, or delete issues, comments, decision requests, or chat messages.

Run each case from its named existing chat/DM decision card. Tap its plain `Comment` link. Do not tap an Issue chip, paste a URL, or navigate by search. If the required existing card is unavailable, record `NOT-RUN` and stop that case; do not create a substitute card.

| Case | Source card | Exact Artemis goal | Expected issue and linked-comment evidence |
| --- | --- | --- | --- |
| A | Existing Cheese Work DM decision request `32839d38-d445-48c5-952c-4ac477bc24ba`, asking whether to accept the CHE-3357 replay/sample proposal. | `Open the existing Cheese Work DM decision request 32839d38-d445-48c5-952c-4ac477bc24ba that asks whether to accept the CHE-3357 replay/sample proposal. Tap only its plain Comment link. Wait until the issue screen settles. Make no changes.` | Target issue UUID `01a1a0eb-718d-778e-9948-770bc5e04ad7`; `CHE-3357`, `feat(multica): replay ask-guard signals on 7 days and sample the residual gap`; linked timeline comment `01a1a13c-c53d-77b5-9d25-052e16e6cc18`, identified by its attached `replay.py` and `sample.py` files. |
| B | Existing Cheese Work DM decision card posted 4 Nov 2026, asking Cheese to accept the frozen CHE-1554 r5 revision 2 owner card. Decision request `9d7091c7-5e0b-48db-8ba3-7921f36d128d`; visible text starts “Do you accept the frozen r5 revision 2 owner card…?” | `Open the existing Cheese Work DM decision card posted 4 Nov 2026 that asks “Do you accept the frozen r5 revision 2 owner card, its candidate and presentation tradeoff, the exact tested-binary compatibility scope, and r5-first ordering for one owner-only transaction?” (decision request 9d7091c7-5e0b-48db-8ba3-7921f36d128d). Tap only its plain Comment link. Wait until the issue screen settles. Make no changes.` | Target issue UUID `01a12f17-2493-7dff-a202-cac3a562ff36`; `CHE-1554`, `chore(instructions): require markdown links for issues, PRs and files in human-facing text`; linked timeline comment `01a1a4c9-f3e5-76a9-8231-382d81aee361`, identified by its text “Chose: Accept card” and its answer link to the decision card. |

Set `ARTEMIS_GOAL` to the exact goal in the selected row. Set `ANDROID_SERIAL` to the exact granted serial, `ARTEMIS_TEST_NAME` to a unique case name, `ARTEMIS_RUN_ID` to a separate unique batch/run identifier, and `ARTIFACT_DIR` to the run's evidence directory. Run Artemis from the pinned checkout with authorized host configuration:

```sh
uv run artemis run "$ARTEMIS_GOAL" --standalone --profile pro --verification-level strict --device-serial "$ANDROID_SERIAL" --test-name "$ARTEMIS_TEST_NAME" --run-id "$ARTEMIS_RUN_ID" --traces-path "$ARTIFACT_DIR/artemis-traces"
```

Use standalone mode so verification, test name, run ID, and trace flags apply to this run. If Artemis routes to or starts the shared daemon, stop and record the attempt as `FAIL`; do not stop a pre-existing shared daemon. Record process exit status, Artemis task/report identifiers, runner checkout SHA, configured model ID, and generated trace paths. Do not include API keys. Do not use model narration as the UI assertion.

Confirm the tapped link target by the resulting issue identifier and exact title in the case row. The tapped `href` is not exposed in the Android accessibility hierarchy. A mismatch is `FAIL`; do not continue that case. For each fixed-build case, Luna's screenshot oracle must confirm the complete issue-detail screen: the expected identifier is in the native navigation header, the exact title is in the `IssueHeaderCard` body, the expected linked timeline comment is visible, and no not-found/load-error message is present. For Case A, confirm the comment by its attached `replay.py` and `sample.py` files. For Case B, confirm “Chose: Accept card” and its answer link to the decision card. Record each location and visible-text check as PASS/FAIL. A screenshot that does not show all four checks is FAIL. Sol signs the returned execution evidence when verifying the run. Capture only the target issue content needed for this check.

## Independent UI assertion

Artemis navigation and its final report are not the assertion. After navigation, capture the Android accessibility hierarchy and screenshot for the visible screen. The hierarchy is diagnostic evidence only; its text matches do not establish that the native navigation header or `IssueHeaderCard` is visible. Luna's screenshot review is the authoritative screen and content oracle, including the issue identifier's header location, exact title's `IssueHeaderCard` location, the case-specific linked comment, and absence of not-found/load-error messages. Set `ANDROID_SERIAL`, `ARTEMIS_RUN_ID`, and `ARTIFACT_DIR` as specified above. The commands set the output paths and use a unique temporary file on the device:

```sh
set -e
mkdir -p "$ARTIFACT_DIR"
UI_XML="$ARTIFACT_DIR/${ARTEMIS_RUN_ID}.xml"
SCREENSHOT="$ARTIFACT_DIR/${ARTEMIS_RUN_ID}.png"
REMOTE_XML="/sdcard/che-3531-${ARTEMIS_RUN_ID}.xml"
adb -s "$ANDROID_SERIAL" shell test ! -e "$REMOTE_XML" || exit 2
trap 'adb -s "$ANDROID_SERIAL" shell rm "$REMOTE_XML"' EXIT
adb -s "$ANDROID_SERIAL" shell uiautomator dump "$REMOTE_XML"
adb -s "$ANDROID_SERIAL" exec-out cat "$REMOTE_XML" > "$UI_XML"
adb -s "$ANDROID_SERIAL" exec-out screencap -p > "$SCREENSHOT"
adb -s "$ANDROID_SERIAL" shell rm "$REMOTE_XML"
trap - EXIT
```

Keep the XML and screenshot in the run artifacts. If the remote path already exists, choose a new run identifier; do not overwrite it. Record the cleanup result. Set `ISSUE_IDENTIFIER` and `ISSUE_TITLE` to the expected values from the case row. These whole-document matches are diagnostic only and cannot pass a case:

```sh
grep -Fq -- "$ISSUE_IDENTIFIER" "$UI_XML" && grep -Fq -- "$ISSUE_TITLE" "$UI_XML" && ! grep -Fq -- 'does not exist' "$UI_XML" && ! grep -Fq -- 'issue not found' "$UI_XML"
```

Record the command and exit status as diagnostic evidence. A generic hierarchy node can contain these strings while navigation failed, so the grep result is never the case assertion. The screenshot oracle above controls PASS/FAIL.

## Comparison and qualification sequence

1. Install the pre-fix APK with `adb install -r`. Do not uninstall between builds. If replacement requires uninstall due to signature/package mismatch, mark the affected comparison and subsequent fixed case `NOT-RUN`; do not clear app data or change the authenticated account. Before calling Case B a pre-fix regression observation, verify from existing production metadata that the CHE-1554 regression precondition is present. Do not create or edit production metadata to establish it. If the condition is absent or cannot be verified, mark only that comparison `NOT-RUN` with the reason. Open the existing Comment link for Case A, then Case B when eligible. Record the observed error state and capture one screenshot and hierarchy per issue. The expected Case A comparison is the reported false “does not exist” failure; this is the pre-fix application regression observation, not the deliberate assertion negative control. The hierarchy grep is diagnostic only.
2. Install the fixed APK with `adb install -r`, without uninstalling. Run Cases A and B. Require the screenshot oracle to pass each case; record hierarchy grep output as diagnostic only. Capture one screenshot and hierarchy for each issue.
3. Return to the same source card and repeat Case A from a clean app navigation state without clearing app data. Require the screenshot oracle to pass again and retain the rerun screenshot and hierarchy. This is the clean representative rerun for the API 35 x86_64 first-run class qualification.
4. **Qualification blocker: pinned Artemis cannot currently demonstrate the required failing exit for an assertion failure.** At runner `cbf2fea84cbd47ea54053239679ab521b01d4d23`, `artemis/sdk/agent.py` logs `tests_failed > 0`, finalizes the task without an error, and ends the session as `completed`; `artemis/interfaces/cli/commands/run.py` does not inspect the returned assertion summary. A strict Checker assertion failure can therefore produce a successful CLI exit. A shell inversion or a wrapper that changes that exit does not prove Artemis reports a failing exit. Do not claim the API 35 x86_64 class is qualified until the pinned runner or an approved report-gating path provides an Artemis-run-linked failing exit and preserves the report and traces. Record this qualification requirement as `BLOCKED`, with the run/report identifiers and artifact paths if an attempt is later authorized. Do not alter the runner in this case.

The pre-fix comparison, fixed-build pass, clean rerun, and any future Artemis-reported controlled failure are separate attempts. Never change the testcase expectation to turn an unexpected result into a pass. If the accessibility hierarchy cannot expose the issue identifier and title, retain it as diagnostic evidence and use the screenshot oracle; missing or unreadable screenshot evidence is `BLOCKED`.

## Setup, synchronization, and cleanup

- Before every attempt, confirm the installed APK SHA-256 and app source pin in the report. Poll for up to 60 seconds by repeating `adb -s "$ANDROID_SERIAL" shell uiautomator dump "$REMOTE_XML"` and pulling the hierarchy. Each dump may overwrite only this run's unique `REMOTE_XML`; do not overwrite a pre-existing path. Treat dump failure, including a null root node, as retryable until the deadline. Stop polling when the target issue screen is visually settled; hierarchy text is diagnostic and cannot establish screen identity. Record timeout as `FAIL` with the last observed state.
- Confirm the app has an authorized, already authenticated account after each `adb install -r`. Do not put credentials in the case, command history, logs, screenshots, or report. If authentication is unavailable, record `NOT-RUN`.
- Reset between attempts by returning to the same existing source chat/card and reopening the plain `Comment` link. Do not clear app data, reset or reboot the AVD, restart shared ADB, or modify account/workspace data.
- Target all device commands at the exact serial from the active grant. Use a unique, task-scoped temporary hierarchy path and remove that exact path after collection. Stop only processes started by this run and record cleanup and release evidence for Sol.
- Preserve Artemis run reports, commands, exit statuses, screenshots, hierarchy captures, retry outcomes, app APK hashes, runner/testcase pins, and cleanup evidence on CHE-3531. Redact unrelated issue/chat content before attaching artifacts.

## Result states

- Fixed-build case: `PASS` only when Luna's screenshot review confirms the expected identifier in the native navigation header, exact title in `IssueHeaderCard`, the named linked comment, and absence of not-found/load-error messages.
- Pre-fix comparison: record the observed error; never label it as the deliberate assertion failure.
- Clean rerun: `PASS` only when the same fixed-build screenshot oracle passes again after the defined navigation reset.
- Artemis failure-path qualification: remains `BLOCKED` until an Artemis-run-linked controlled assertion failure has a failing exit and its report and traces are preserved. Shell inversion alone does not qualify the runner.
- Missing grant, missing source card, missing artifact, uncertain cleanup, or unavailable target: `NOT-RUN` or `BLOCKED`, never `PASS`.

## References

- PR #918 documents the `metadata.decision_park` parse failure and the two issue repros.
- `apps/mobile/app/(app)/[workspace]/issue/[id].tsx` renders the issue detail and loading/error states.
- `apps/mobile/components/issue/issue-header-card.tsx` renders the issue identifier and title locations checked by Luna's screenshot oracle.
- Android execution, artifact, qualification, and cleanup requirements are governed by the live Android device squad grant and policy.
