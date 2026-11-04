# CHE-3531: plain Comment link opens a parked issue

## Immutable inputs

- Runner source: `cheese-work/artemis` commit `cbf2fea84cbd47ea54053239679ab521b01d4d23` (Artemis Pro; pinned source only, this case does not claim device-class qualification).
- Fixed app source: `cheese-work/multica` commit `b9715570402d6d38e3a8dce12317b5a8724c7329` (PR #918).
- Pre-fix app source: `cheese-work/multica` commit `1a9e20e40d84916e0bca3b1358e77be7de22565b` (parent of the first fix commit).
- Requested target class: X99 AVD `x99_api35_medium_phone`, API 35, x86_64. The executor must use the exact serial and actions granted by the Android device squad on CHE-3531.
- Keep the built APK SHA-256, build command, runner checkout SHA, device serial, testcase Git SHA, and testcase file SHA-256 in the execution report.

## Cases

Use an already authenticated, authorized workspace account. Do not record credentials or unrelated personal data. Do not create, edit, answer, or delete issues, comments, decision requests, or chat messages.

Run each case from the existing chat/DM decision card. Tap its plain `Comment` link. Do not tap an Issue chip, paste a URL, or navigate by search. If the required existing card is unavailable, record `NOT-RUN` and stop that case; do not create a substitute card.

Run Artemis from the pinned checkout with the authorized host configuration. Set `ANDROID_SERIAL` to the exact granted serial and `ARTEMIS_TASK` to the matching case above. Run:

```sh
uv run artemis run "$ARTEMIS_TASK" --profile pro --verification-level strict --device-serial "$ANDROID_SERIAL"
```

Record the process exit status and Artemis task/report identifiers. Artemis must identify the target card, tap the plain `Comment` link, and stop if the link target does not match the case UUID. Do not use model narration as the UI assertion.

| Case | Decision-card target | Expected fixed-build issue |
| --- | --- | --- |
| A | Decision request `32839d38-d445-48c5-952c-4ac477bc24ba`; target issue UUID `01a1a0eb-718d-778e-9948-770bc5e04ad7` | `CHE-3357`, `feat(multica): replay ask-guard signals on 7 days and sample the residual gap` |
| B | Existing decision card whose plain `Comment` link targets issue UUID `01a12f17-2493-7dff-a202-cac3a562ff36` | `CHE-1554`, `chore(instructions): require markdown links for issues, PRs and files in human-facing text` |

For each fixed-build case, wait for the loading state to settle. The native issue header and body must both show the expected issue identifier, and the body must show the exact title in the table. The opened timeline must show the comment reached by the tapped link. Capture a screenshot with the issue identifier, title, and linked comment visible. Capture only the target issue content needed for this check.

## Independent UI assertion

Artemis navigation and its final report are not the assertion. After navigation, capture the Android accessibility hierarchy and screenshot for the visible screen. Set `ANDROID_SERIAL` to the exact granted serial, `ARTEMIS_RUN_ID` to a unique run identifier, and `ARTIFACT_DIR` to the run's evidence directory. The commands set the output paths and use a unique temporary file on the device:

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

Keep the XML and screenshot in the run artifacts. If the remote path already exists, choose a new run identifier; do not overwrite it. Record the cleanup result. With `ISSUE_IDENTIFIER` and `ISSUE_TITLE` set to the expected values from the case table, run the positive assertion for each fixed-build case:

```sh
grep -Fq -- "$ISSUE_IDENTIFIER" "$UI_XML" && grep -Fq -- "$ISSUE_TITLE" "$UI_XML"
```

Both matches are required. A missing match is a failing assertion. Record the command and exit status. Keep the hierarchy and screenshot as evidence.

## Comparison and qualification sequence

1. Install the pre-fix app build under the issue-visible grant. Open the existing Comment link for CHE-3357, then CHE-1554. Record the observed error state and capture one screenshot and hierarchy per issue. The expected comparison is the reported false “does not exist” failure; this is the pre-fix application regression observation, not the deliberate assertion negative control.
2. Install the fixed app build under the grant. Run Cases A and B and require both positive UI assertions to exit 0. Capture one screenshot and hierarchy for each issue.
3. Return to the same source card and repeat Case A from a clean app navigation state without clearing app data. Require the positive UI assertion to exit 0 and retain the rerun screenshot and hierarchy. This is the clean representative rerun for the API 35 x86_64 first-run class qualification.
4. On the fixed build, deliberately run Case A's positive assertion against an expectation that the target is absent:

   ```sh
   ! grep -Fq -- "$ISSUE_IDENTIFIER" "$UI_XML"
   ```

   The assertion must exit non-zero because the fixed screen contains the target. Record this as `EXPECTED ASSERTION FAILURE` with its command and exit status. It is not an app regression and must not be counted as one.

The pre-fix comparison, fixed-build pass, clean rerun, and deliberate assertion failure are separate attempts. Never change the testcase expectation to turn an unexpected result into a pass. If the accessibility hierarchy cannot expose the issue identifier and title, record the limitation as `BLOCKED`; do not replace the oracle with Artemis narration alone.

## Setup, synchronization, and cleanup

- Before every attempt, confirm the installed APK SHA-256 and app source pin in the report. Wait for the issue screen to finish loading before taking a screenshot or hierarchy capture; allow up to 60 seconds, then record timeout as `FAIL` or `NOT-RUN` with the observed state.
- Confirm the app has an authorized, already authenticated account after each app install. Do not put credentials in the case, command history, logs, screenshots, or report. If authentication is unavailable, record `NOT-RUN`.
- Reset between attempts by returning to the same existing source chat/card and reopening the plain `Comment` link. Do not clear app data, reset or reboot the AVD, restart shared ADB, or modify account/workspace data.
- Target all device commands at the exact serial from the active grant. Use a unique, task-scoped temporary hierarchy path and remove that exact path after collection. Stop only processes started by this run and record cleanup and release evidence for Sol.
- Preserve Artemis run reports, commands, exit statuses, screenshots, hierarchy captures, retry outcomes, app APK hashes, runner/testcase pins, and cleanup evidence on CHE-3531. Redact unrelated issue/chat content before attaching artifacts.

## Result states

- Fixed-build positive assertion: `PASS` only when both exact strings are present and the linked comment is visible.
- Pre-fix comparison: record the observed error; never label it as the deliberate assertion failure.
- Clean rerun: `PASS` only when the same fixed-build positive assertion passes again after the defined navigation reset.
- Deliberate negative control: `EXPECTED ASSERTION FAILURE` only when the inverted assertion exits non-zero while the fixed screen still shows the expected issue.
- Missing grant, missing source card, missing artifact, uncertain cleanup, or unavailable target: `NOT-RUN` or `BLOCKED`, never `PASS`.

## References

- PR #918 documents the `metadata.decision_park` parse failure and the two issue repros.
- `apps/mobile/app/(app)/[workspace]/issue/[id].tsx` renders the issue detail and loading/error states.
- `apps/mobile/components/issue/issue-header-card.tsx` renders the issue identifier and title used by the independent UI assertion.
- Android execution, artifact, qualification, and cleanup requirements are governed by the live Android device squad grant and policy.
