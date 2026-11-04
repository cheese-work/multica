# CHE-3531: plain Comment link opens a parked issue

## Immutable inputs

- Runner source: `cheese-work/artemis` commit `cbf2fea84cbd47ea54053239679ab521b01d4d23` (Artemis Pro; pinned source only, this case does not claim device-class qualification).
- Runner dependency lock: `cheese-work/artemis/uv.lock` at the runner commit above (Git blob `dc2d35b8c284055c92ff1cd2560c8201e648bf9f`).
- Runner CLI configuration: `cheese-work/artemis/pyproject.toml` at the runner commit above (Git blob `a36b834875b465bf33e221ffcffc003d4ca94ec4`).
- Fixed app source: `cheese-work/multica` commit `ef61b6263cdc48f416458e836797f985bf1f6a57` (PR #918).
- Pre-fix app source: `cheese-work/multica` commit `1a9e20e40d84916e0bca3b1358e77be7de22565b` (parent of the first fix commit).
- Requested target class: X99 AVD `x99_api35_medium_phone`, API 35, x86_64. The executor must use the exact serial and actions granted by the Android device squad on CHE-3531.
- Keep the built APK SHA-256, build command, runner checkout SHA, device serial, testcase Git SHA, and testcase file SHA-256 in the execution report.

## Reproducible APK inputs

Build each source pin in a clean checkout on the same X99 Linux x86_64 host with Node.js `22.23.1`, pnpm `10.28.2`, OpenJDK `21.0.12.1`, Android SDK platform `35` revision `2`, Build Tools `35.0.0`, NDK `28.2.13676358`, and that source revision's Gradle wrapper. Use the lockfile at each source pin and `pnpm install --frozen-lockfile`. For both builds, export `APP_ENV=production`, `EXPO_PUBLIC_API_URL=https://api.multica.ai`, and `EXPO_PUBLIC_WEB_URL=https://multica.ai`. Do not set credentials or include them in evidence.

For each checkout, run this same recipe:

```sh
export APP_ENV=production
export EXPO_PUBLIC_API_URL=https://api.multica.ai
export EXPO_PUBLIC_WEB_URL=https://multica.ai
pnpm install --frozen-lockfile
pnpm --filter @multica/mobile exec expo prebuild --clean --platform android --no-install
```

In the generated `apps/mobile/android/app/build.gradle`, set the single `defaultConfig.applicationId` property to `ai.multica.mobile`; stop as `NOT-RUN` if that property is absent or ambiguous. This generated-file edit is identical for both builds and is not a source change. From `apps/mobile/android`, run `./gradlew :app:assembleRelease -PreactNativeArchitectures=x86_64`. Verify the APK reports package `ai.multica.mobile` and ABI `x86_64`; record the complete tool versions, source SHA, lockfile SHA, command, package ID, ABI, and APK SHA-256 for each build. Do not proceed if either build cannot use these exact inputs.

Install each APK with `adb -s "$ANDROID_SERIAL" install -r`. If Android reports `INSTALL_FAILED_VERSION_DOWNGRADE`, or the package/signature prevents replacement, mark that install and dependent comparison `NOT-RUN`. Do not uninstall, clear app data, or use a downgrade workaround.

## Cases

Use an already authenticated, authorized workspace account. Do not record credentials or unrelated personal data. Do not create, edit, answer, or delete issues, comments, decision requests, or chat messages.

Run each case from its named existing chat/DM decision card. Tap its plain `Comment` link. Do not tap an Issue chip, paste a URL, or navigate by search. If the required existing card is unavailable, record `NOT-RUN` and stop that case; do not create a substitute card.

| Case | Source card | Exact Artemis goal | Expected issue and linked-comment evidence |
| --- | --- | --- | --- |
| A | Existing Cheese Work DM decision request `32839d38-d445-48c5-952c-4ac477bc24ba`, asking whether to accept the CHE-3357 replay/sample proposal. Its `ping_message_id` is `01a1a166-673d-79bc-a412-d7a557df410c`. The exact message body links its plain `Comment` to `/cheese-work/issues/01a1a0eb-718d-778e-9948-770bc5e04ad7#comment-01a1a165-1b88-769c-8137-bd8be641b61b`. | `Open the existing Cheese Work DM decision request 32839d38-d445-48c5-952c-4ac477bc24ba that asks whether to accept the CHE-3357 replay/sample proposal. Tap only the plain Comment link whose target ends in comment-01a1a165-1b88-769c-8137-bd8be641b61b. Wait until the issue screen settles. Make no data changes.` | Target issue UUID `01a1a0eb-718d-778e-9948-770bc5e04ad7`; `CHE-3357`, `feat(multica): replay ask-guard signals on 7 days and sample the residual gap`; linked timeline comment `01a1a165-1b88-769c-8137-bd8be641b61b`, identified by `Verdict: BLOCKED / NOT-RUN` and reviewer `x99-codex-terra`. |
| B | Existing Cheese Work DM decision card posted 4 Nov 2026, asking Cheese to accept the frozen CHE-1554 r5 revision 2 owner card. Decision request `9d7091c7-5e0b-48db-8ba3-7921f36d128d`; its `ping_message_id` is `01a1a493-3dd7-79c9-8c39-1ca017a18299`. The exact message body links its plain `Comment` to `/cheese-work/issues/01a12f17-2493-7dff-a202-cac3a562ff36#comment-01a1a48c-0e98-7177-bf6e-3fcbd96afb5c`. | `Open the existing Cheese Work DM decision card posted 4 Nov 2026 that asks “Do you accept the frozen r5 revision 2 owner card, its candidate and presentation tradeoff, the exact tested-binary compatibility scope, and r5-first ordering for one owner-only transaction?” (decision request 9d7091c7-5e0b-48db-8ba3-7921f36d128d). Tap only the plain Comment link whose target ends in comment-01a1a48c-0e98-7177-bf6e-3fcbd96afb5c. Wait until the issue screen settles. Make no data changes.` | Target issue UUID `01a12f17-2493-7dff-a202-cac3a562ff36`; `CHE-1554`, `chore(instructions): require markdown links for issues, PRs and files in human-facing text`; linked timeline comment `01a1a48c-0e98-7177-bf6e-3fcbd96afb5c`, identified by the opening text `Blocked step: agent-actionable.` |

Set `ARTEMIS_GOAL` to the exact goal in the selected row. Set `ANDROID_SERIAL` to the exact granted serial, `ARTEMIS_TEST_NAME` to a unique case name, `ARTEMIS_RUN_ID` to a separate unique batch/run identifier, and `ARTIFACT_DIR` to the run's evidence directory. Run Artemis from the pinned checkout with authorized host configuration:

Before Artemis starts, capture the source card and plain Comment link:

```sh
mkdir -p "$ARTIFACT_DIR"
SOURCE_SCREENSHOT="$ARTIFACT_DIR/${ARTEMIS_RUN_ID}-source.png"
adb -s "$ANDROID_SERIAL" exec-out screencap -p > "$SOURCE_SCREENSHOT"
```

```sh
uv run artemis run "$ARTEMIS_GOAL" --standalone --profile pro --verification-level strict --device-serial "$ANDROID_SERIAL" --test-name "$ARTEMIS_TEST_NAME" --run-id "$ARTEMIS_RUN_ID" --traces-path "$ARTIFACT_DIR/artemis-traces"
```

Use standalone mode so verification, test name, run ID, and trace flags apply to this run. If Artemis routes to or starts the shared daemon, stop and record the attempt as `FAIL`; do not stop a pre-existing shared daemon. Record process exit status, Artemis task/report identifiers, runner checkout SHA, configured model ID, and generated trace paths. Do not include API keys. Do not use model narration as the UI assertion.

The decision records' `ask` fields returned through `multica decision get` are bound to the exact source messages by matching `ping_message_id`; those bodies establish the hrefs above. Do not substitute `issue_comment_id`, `source_comment_id`, a nearby reply, or an answer comment for the href target. Confirm the resulting issue UUID and exact title against the selected case row. A mismatch is `FAIL`; do not continue that case.

For every attempt, the source-card screenshot must show enough of the named decision text and plain `Comment` link to identify the source card. Redact unrelated chat content. The Artemis goal stops on the issue screen after navigation settles; it does not scroll. Immediately after Artemis exits, capture and preserve the first settled issue screen before any scroll or other inspection. This initial-landing screenshot must show the expected named comment and no not-found/load-error message. Then scroll the same issue screen to the top without tapping or navigating, and capture a second screenshot. Luna's visual oracle checks the native navigation header identifier and exact title in the `IssueHeaderCard` on this second screenshot. Bind the source screenshot, initial-landing screenshot, header/title screenshot, hierarchy files, source message ID, href target comment ID, issue UUID, app pin, and Artemis attempt ID in one report row. All four checks (native header identifier, exact title, named linked comment, and absence of load errors) must pass for the same issue and attempt. The two issue screenshots together are the visual oracle; a single viewport need not contain the scrolling title and linked comment.

For Case A, the named linked-comment check is `Verdict: BLOCKED / NOT-RUN` by `x99-codex-terra`. For Case B, it is the `Blocked step: agent-actionable.` opening text on the linked comment. Record each screenshot filename and the visible text/location as PASS/FAIL. Sol signs the returned execution evidence when verifying the run. Capture only the target issue content needed for this check.

## Independent UI assertion

Artemis navigation and its final report are not the assertion. Capture the source-card screenshot before the run, then the first settled linked-comment landing screenshot immediately after the run, then the header/title screenshot after scrolling the same issue screen. The hierarchy is diagnostic evidence only; its text matches do not establish that the native navigation header or `IssueHeaderCard` is visible. Luna's screenshot review is the authoritative screen and content oracle. Inspect the Artemis trace for the same attempt and confirm it tapped only the named plain `Comment` link. Record the later scroll as an evidence-collection action outside the Artemis navigation trace. Any other Artemis tap or route change is `FAIL`. Set `ANDROID_SERIAL`, `ARTEMIS_RUN_ID`, and `ARTIFACT_DIR` as specified above. Use unique local and remote artifact names for each capture and a unique temporary XML path on the device:

```sh
set -e
mkdir -p "$ARTIFACT_DIR"
UI_XML="$ARTIFACT_DIR/${ARTEMIS_RUN_ID}-landing.xml"
LANDING_SCREENSHOT="$ARTIFACT_DIR/${ARTEMIS_RUN_ID}-landing.png"
HEADER_SCREENSHOT="$ARTIFACT_DIR/${ARTEMIS_RUN_ID}-header.png"
REMOTE_XML="/sdcard/che-3531-${ARTEMIS_RUN_ID}.xml"
adb -s "$ANDROID_SERIAL" shell test ! -e "$REMOTE_XML" || exit 2
trap 'adb -s "$ANDROID_SERIAL" shell rm "$REMOTE_XML"' EXIT
DEADLINE=$((SECONDS + 60))
while (( SECONDS < DEADLINE )); do
  if adb -s "$ANDROID_SERIAL" shell uiautomator dump "$REMOTE_XML" >/dev/null 2>&1 \
    && adb -s "$ANDROID_SERIAL" exec-out cat "$REMOTE_XML" > "$UI_XML" \
    && grep -Fq -- "$ISSUE_IDENTIFIER" "$UI_XML"; then
    break
  fi
  sleep 2
done
if ! grep -Fq -- "$ISSUE_IDENTIFIER" "$UI_XML"; then
  echo "FAIL: issue identifier not observed before the 60-second deadline" >&2
  exit 1
fi
adb -s "$ANDROID_SERIAL" exec-out screencap -p > "$LANDING_SCREENSHOT"
adb -s "$ANDROID_SERIAL" shell rm "$REMOTE_XML"
trap - EXIT
```

Run the source `screencap` before Artemis starts. Run the hierarchy poll and landing `screencap` immediately after Artemis exits, before any scroll. Then scroll to the top of the same issue screen without tapping and save `$HEADER_SCREENSHOT`. If the remote path already exists, choose a new run identifier; do not overwrite it. Record cleanup. Set `ISSUE_IDENTIFIER` and `ISSUE_TITLE` to the expected values from the case row before polling. The identifier match only synchronizes collection; it is not an assertion. These whole-document matches are diagnostic only and cannot pass a case:

```sh
grep -Fq -- "$ISSUE_IDENTIFIER" "$UI_XML" && grep -Fq -- "$ISSUE_TITLE" "$UI_XML" && ! grep -Fq -- 'does not exist' "$UI_XML" && ! grep -Fq -- 'issue not found' "$UI_XML"
```

Record the command and exit status as diagnostic evidence. A generic hierarchy node can contain these strings while navigation failed, so the grep result is never the case assertion. The screenshot oracle above controls PASS/FAIL.

## Comparison and qualification sequence

1. Install the pre-fix APK with `adb install -r`. Do not uninstall between builds. If install returns `INSTALL_FAILED_VERSION_DOWNGRADE`, or package/signature mismatch prevents replacement, mark that install and dependent comparisons `NOT-RUN`; do not clear app data or change the authenticated account. Before the Case A pre-fix attempt, read only the existing `metadata.decision_park` precondition and record whether it is still an object. If it is absent, changed, or cannot be verified at the attempt, mark the Case A pre-fix comparison `NOT-RUN`; do not create or edit metadata. For Case B, verify its own existing regression precondition from production metadata before any pre-fix comparison. If it is absent or cannot be verified, mark only that comparison `NOT-RUN`. Capture the source-card screenshot, first landing screenshot, and hierarchy for each eligible pre-fix attempt. The expected Case A comparison is the reported false “does not exist” failure; it is not the deliberate assertion negative control. The hierarchy grep is diagnostic only.
2. Install the fixed APK with `adb install -r`, without uninstalling. Run Cases A and B. Require the screenshot oracle to pass each eligible case; record hierarchy grep output as diagnostic only. Capture source, first landing, and header/title screenshots plus hierarchy for each attempt.
3. Return to the same source card and repeat Case A from a clean app navigation state without clearing app data. Require the screenshot oracle to pass again and retain all three screenshots and hierarchy. This is a clean runner-stability rerun and representative pass for the API 35 x86_64 first-run class qualification. It is limited regression evidence because the app may return to cached issue data; it does not establish a new fetch or parse.
4. **Qualification blocker: pinned Artemis cannot currently demonstrate the required failing exit for an assertion failure.** At runner `cbf2fea84cbd47ea54053239679ab521b01d4d23`, `artemis/sdk/agent.py` logs `tests_failed > 0`, finalizes the task without an error, and ends the session as `completed`; `artemis/interfaces/cli/commands/run.py` does not inspect the returned assertion summary. A strict Checker assertion failure can therefore produce a successful CLI exit. A shell inversion or a wrapper that changes that exit does not prove Artemis reports a failing exit. Do not claim the API 35 x86_64 class is qualified until the pinned runner or an approved report-gating path provides an Artemis-run-linked failing exit and preserves the report and traces. Record this qualification requirement as `BLOCKED`, with the run/report identifiers and artifact paths if an attempt is later authorized. Do not alter the runner in this case.

The pre-fix comparison, fixed-build pass, clean rerun, and any future Artemis-reported controlled failure are separate attempts. Never change the testcase expectation to turn an unexpected result into a pass. If the accessibility hierarchy cannot expose the issue identifier and title, retain it as diagnostic evidence and use the screenshot oracle; missing or unreadable screenshot evidence is `BLOCKED`.

## Setup, synchronization, and cleanup

- Before every attempt, confirm the installed APK SHA-256 and app source pin in the report. Capture the named source card before tapping. Poll for up to 60 seconds by repeating `adb -s "$ANDROID_SERIAL" shell uiautomator dump "$REMOTE_XML"` and pulling the hierarchy. Each dump may overwrite only this run's unique `REMOTE_XML`; do not overwrite a pre-existing path. Treat dump failure, including a null root node, as retryable until the deadline. Stop polling when the target issue screen is visually settled. Immediately capture the first landing screenshot before scrolling. Record timeout as `FAIL` with the last observed state.
- Confirm the app has an authorized, already authenticated account after each `adb install -r`. Do not put credentials in the case, command history, logs, screenshots, or report. If authentication is unavailable, record `NOT-RUN`.
- Reset between attempts by returning to the same existing source chat/card and reopening the plain `Comment` link. Do not clear app data, reset or reboot the AVD, restart shared ADB, or modify account/workspace data.
- Target all device commands at the exact serial from the active grant. Use a unique, task-scoped temporary hierarchy path and remove that exact path after collection. Stop only processes started by this run and record cleanup and release evidence for Sol.
- Preserve Artemis run reports, commands, exit statuses, screenshots, hierarchy captures, retry outcomes, app APK hashes, runner/testcase pins, and cleanup evidence on CHE-3531. Redact unrelated issue/chat content before attaching artifacts.

## Result states

- Fixed-build case: `PASS` only when Luna's same-attempt visual review confirms the expected identifier in the native navigation header and exact title in `IssueHeaderCard` after scroll-to-top, the actual href-target comment in the preserved initial-landing screenshot, and absence of not-found/load-error messages in both issue screenshots. The source-card screenshot must show the named source text and plain Comment link.
- Pre-fix comparison: record the observed error; never label it as the deliberate assertion failure.
- Clean rerun: `PASS` only when the same fixed-build screenshot oracle passes again after the defined navigation reset.
- Artemis failure-path qualification: remains `BLOCKED` until an Artemis-run-linked controlled assertion failure has a failing exit and its report and traces are preserved. Shell inversion alone does not qualify the runner.
- Missing grant, missing source card, missing artifact, uncertain cleanup, or unavailable target: `NOT-RUN` or `BLOCKED`, never `PASS`.

## References

- PR #918 documents the `metadata.decision_park` parse failure and the two issue repros.
- `apps/mobile/app/(app)/[workspace]/issue/[id].tsx` renders the issue detail and loading/error states.
- `apps/mobile/components/issue/issue-header-card.tsx` renders the issue identifier and title locations checked by Luna's screenshot oracle.
- Android execution, artifact, qualification, and cleanup requirements are governed by the live Android device squad grant and policy.
