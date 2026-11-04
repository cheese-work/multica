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

Build each source pin in a clean checkout on the same X99 Linux x86_64 host with Node.js `22.23.1`, pnpm `10.28.2` provisioned through Corepack, OpenJDK `21.0.12.1`, Android SDK platform `35` revision `2`, Build Tools `35.0.0`, NDK `28.2.13676358`, and that source revision's Gradle wrapper. Use the lockfile at each source pin and `pnpm install --frozen-lockfile`. For both builds, export `APP_ENV=production`, `EXPO_PUBLIC_API_URL=https://congvc-c00.taila6fa8a.ts.net:19444`, and `EXPO_PUBLIC_WEB_URL=https://congvc-c00.taila6fa8a.ts.net:19443`. The mobile API client concatenates `EXPO_PUBLIC_API_URL` with `/api/...` paths (`apps/mobile/data/api.ts`); do not add `/api` to the origin. Do not set credentials or include them in evidence.

For each checkout, run this same recipe:

```sh
export APP_ENV=production
export EXPO_PUBLIC_API_URL=https://congvc-c00.taila6fa8a.ts.net:19444
export EXPO_PUBLIC_WEB_URL=https://congvc-c00.taila6fa8a.ts.net:19443
corepack prepare pnpm@10.28.2 --activate
pnpm install --frozen-lockfile
pnpm --filter @multica/mobile exec expo prebuild --clean --platform android --no-install
```

The source `apps/mobile/app.config.ts` does not define `android.package`. Record that fact and the public Expo config's generated package value before changing the generated project. If prebuild does not complete without an unpinned source change, mark the build `NOT-RUN`; do not edit app source. In generated `apps/mobile/android/app/build.gradle`, set the single `defaultConfig.applicationId` property to `ai.multica.mobile`; stop as `NOT-RUN` if the property is absent or ambiguous. Record generated `compileSdkVersion`, `targetSdkVersion`, and `buildToolsVersion` from Gradle; platform `35` is the installed API target, not an assertion that generated compile SDK resolves to 35. This generated-file edit is identical for both builds and is not a source change. From `apps/mobile/android`, run `./gradlew :app:assembleRelease -PreactNativeArchitectures=x86_64`. Verify the APK reports package `ai.multica.mobile` and ABI `x86_64`; record complete tool versions, source SHA, lockfile SHA, command, package ID, ABI, resolved SDK values, and APK SHA-256 for each build. Do not proceed if either build cannot use these exact inputs.

For later granted execution, verify TLS/HTTP reachability from the AVD to both origins, then use an unauthenticated API request to confirm the API route responds without logging authorization headers or response bodies. The API client concatenates the origin with `/api/...` paths. Confirm the pre-existing app session is authenticated in the authorized workspace without opening or recording credentials; if origins or the existing session are unavailable, record `NOT-RUN`. Device reachability and authenticated-app readiness are NOT-RUN for this candidate.

Install each APK with `adb -s "$ANDROID_SERIAL" install -r`. If Android reports `INSTALL_FAILED_VERSION_DOWNGRADE`, or the package/signature prevents replacement, mark that install and dependent comparison `NOT-RUN`. Do not uninstall, clear app data, or use a downgrade workaround.

## Cases

Use an already authenticated, authorized workspace account. Do not record credentials or unrelated personal data. Do not create, edit, answer, or delete issues, comments, decision requests, or chat messages.

Run each case from its named existing chat/DM decision card. Tap its plain `Comment` link. Do not tap an Issue chip, paste a URL, or navigate by search. If the required existing card is unavailable, record `NOT-RUN` and stop that case; do not create a substitute card.

| Case | Source card | Exact Artemis goal | Expected issue and linked-comment evidence |
| --- | --- | --- | --- |
| A | Existing Cheese Work DM decision request `32839d38-d445-48c5-952c-4ac477bc24ba`, asking whether to accept the CHE-3357 replay/sample proposal. Its `ping_message_id` is `01a1a166-673d-79bc-a412-d7a557df410c`. The exact message body links its plain `Comment` to `/cheese-work/issues/01a1a0eb-718d-778e-9948-770bc5e04ad7#comment-01a1a165-1b88-769c-8137-bd8be641b61b`. | `Open the existing Cheese Work DM decision request 32839d38-d445-48c5-952c-4ac477bc24ba that asks whether to accept the CHE-3357 replay/sample proposal. Tap only the plain Comment link whose target ends in comment-01a1a165-1b88-769c-8137-bd8be641b61b. Wait until the issue screen settles. Make no data changes.` | Target issue UUID `01a1a0eb-718d-778e-9948-770bc5e04ad7`; `CHE-3357`, `feat(multica): replay ask-guard signals on 7 days and sample the residual gap`; linked timeline comment `01a1a165-1b88-769c-8137-bd8be641b61b`, identified by `Verdict: BLOCKED / NOT-RUN` and reviewer `x99-codex-terra`. |
| B | Existing Cheese Work DM decision card posted 4 Nov 2026, asking Cheese to accept the frozen CHE-1554 r5 revision 2 owner card. Decision request `9d7091c7-5e0b-48db-8ba3-7921f36d128d`; its `ping_message_id` is `01a1a493-3dd7-79c9-8c39-1ca017a18299`. The exact message body links its plain `Comment` to `/cheese-work/issues/01a12f17-2493-7dff-a202-cac3a562ff36#comment-01a1a48c-0e98-7177-bf6e-3fcbd96afb5c`. | `Open the existing Cheese Work DM decision card posted 4 Nov 2026 that asks “Do you accept the frozen r5 revision 2 owner card, its candidate and presentation tradeoff, the exact tested-binary compatibility scope, and r5-first ordering for one owner-only transaction?” (decision request 9d7091c7-5e0b-48db-8ba3-7921f36d128d). Tap only the plain Comment link whose target ends in comment-01a1a48c-0e98-7177-bf6e-3fcbd96afb5c. Wait until the issue screen settles. Make no data changes.` | Target issue UUID `01a12f17-2493-7dff-a202-cac3a562ff36`; `CHE-1554`, `chore(instructions): require markdown links for issues, PRs and files in human-facing text`; linked timeline comment `01a1a48c-0e98-7177-bf6e-3fcbd96afb5c`, a reply by `x99-codex-sol` under parent `01a1a482-6fdf-7443-abc6-2cef14519f66`, identified by its opening `Verdict: ready for Cheese's owner run.` |

Set `ARTEMIS_GOAL` to the exact goal in the selected row. Set `ANDROID_SERIAL` to the exact granted serial, `ARTEMIS_TEST_NAME` to a unique case name, `ARTEMIS_RUN_ID` to a separate unique batch/run identifier, and `ARTIFACT_DIR` to the run's evidence directory. Run Artemis from the pinned checkout with authorized host configuration:

Position on the source card in a separate Artemis preparation attempt. For Case A, open the named DM request and stop on its decision card. For Case B, open the named 4 Nov 2026 DM card and stop on that card. Save the preparation trace and exit status separately. Capture the source screenshot only after the card is visible and before the navigation attempt. If preparation does not land on the specified card, record `NOT-RUN` and do not start navigation. Do not tap the Comment link during preparation.

Use this exact preparation goal for Case A: `Open existing DM decision request 32839d38-d445-48c5-952c-4ac477bc24ba and stop on its decision card. Do not tap any link and make no data changes.` Use this exact preparation goal for Case B: `Open existing DM decision request 9d7091c7-5e0b-48db-8ba3-7921f36d128d and stop on its 4 Nov 2026 decision card. Do not tap any link and make no data changes.` The navigation goals are the exact goals in the table.

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

For every navigation attempt, the source-card screenshot must show enough of the named decision text and plain `Comment` link to identify the source card. Redact unrelated chat content. Start a separate Artemis navigation attempt from that prepared card. Its goal taps only the named plain Comment link and stops after the first post-tap screen settles; it does not scroll. Immediately after Artemis exits, capture and preserve the first post-tap screen before any hierarchy poll, scroll, or other inspection. A missing issue identifier or hierarchy dump failure must never skip the screenshot. Then collect hierarchy diagnostically, retaining the last successfully pulled non-empty XML. Poll for at most 60 seconds; timeout is navigation `FAIL` and keeps the screenshot plus last available hierarchy, or records hierarchy as unavailable. Separate expected pre-fix error observations from fixed-build failures.

The visual oracle for mobile acceptance is the native issue page render: the native navigation header shows the expected identifier, `IssueHeaderCard` shows the exact title, and the screen has no not-found/load-error text. The initial post-tap screenshot and a later header screenshot together may prove these checks. Do not require automatic scrolling to the linked comment for issue-render PASS. Record the href and expected comment identity as diagnostic evidence. A wrong issue identifier/title or an inert plain Comment link is `FAIL`; if the issue route was not reached, the metadata parsing-regression comparison is `NOT-RUN`. Do not use an Issue chip, search, pasted URL, or edited card.

If the exact title is not visible in the initial screenshot, inspect the same issue screen only after preserving that screenshot. Read `wm size` on the granted serial and calculate `X=width/2`, `Ystart=floor(height*0.75)`, and `Yend=floor(height*0.25)`. Issue at most five `adb -s "$ANDROID_SERIAL" shell input swipe "$X" "$Ystart" "$X" "$Yend" 500` gestures, one at a time, capturing a screenshot after each. Stop when the native header identifier and full exact title are visible, or after five gestures. Do not tap during this evidence scroll. Record each gesture and screenshot filename; if the header/title is still not visible at the bound, the screen oracle is `FAIL`. Bind source-preparation trace/screenshot, navigation trace, initial post-tap screenshot, later header screenshot, hierarchy diagnostics, source message ID, href target comment ID, issue UUID, app pin, and Artemis attempt IDs in one report row.

For Case A, record diagnostic comment identity `01a1a165-1b88-769c-8137-bd8be641b61b`, opening `Verdict: BLOCKED / NOT-RUN`, by `x99-codex-terra`. For Case B, record diagnostic identity `01a1a48c-0e98-7177-bf6e-3fcbd96afb5c`, opening `Verdict: ready for Cheese's owner run.`, by `x99-codex-sol`, as a reply under `01a1a482-6fdf-7443-abc6-2cef14519f66`. Comment identity is diagnostic and does not gate the narrow issue-rendering PASS. Sol signs the returned execution evidence when verifying the run. Capture only the target issue content needed for this check.

## Independent UI assertion

Artemis navigation and its final report are not the assertion. Use separate preparation and navigation attempts. Capture the source-card screenshot after preparation and before navigation. Immediately after the navigation attempt exits, capture the first post-tap screen before hierarchy collection or scrolling. Luna's same-attempt screenshot review checks the native header identifier, exact title in `IssueHeaderCard`, and absence of not-found/load-error text. The hierarchy is diagnostic only. Inspect the navigation trace and confirm it tapped only the named plain `Comment` link. Record evidence-scroll gestures outside the navigation trace. Any other Artemis tap or route change is `FAIL`. Set `ANDROID_SERIAL`, `ARTEMIS_RUN_ID`, and `ARTIFACT_DIR` as specified above. Use unique local and remote artifact names for each capture and a unique temporary XML path on the device:

```sh
mkdir -p "$ARTIFACT_DIR"
UI_XML="$ARTIFACT_DIR/${ARTEMIS_RUN_ID}-landing.xml"
TMP_UI_XML="$ARTIFACT_DIR/${ARTEMIS_RUN_ID}-landing.tmp.xml"
LANDING_SCREENSHOT="$ARTIFACT_DIR/${ARTEMIS_RUN_ID}-landing.png"
REMOTE_XML="/sdcard/che-3531-${ARTEMIS_RUN_ID}.xml"
adb -s "$ANDROID_SERIAL" shell test ! -e "$REMOTE_XML" || exit 2
# Preserve the first post-tap screen before diagnostics, including error pages.
adb -s "$ANDROID_SERIAL" exec-out screencap -p > "$LANDING_SCREENSHOT"
DEADLINE=$((SECONDS + 60))
POLL_RESULT=timeout
while (( SECONDS < DEADLINE )); do
  if adb -s "$ANDROID_SERIAL" shell uiautomator dump "$REMOTE_XML" >/dev/null 2>&1 \
    && adb -s "$ANDROID_SERIAL" exec-out cat "$REMOTE_XML" > "$TMP_UI_XML" \
    && test -s "$TMP_UI_XML"; then
    cp "$TMP_UI_XML" "$UI_XML"
    POLL_RESULT=hierarchy-collected
    if grep -Fq -- "$ISSUE_IDENTIFIER" "$UI_XML"; then
      POLL_RESULT=identifier-observed
      break
    fi
  fi
  sleep 2
done
test -s "$UI_XML" || echo "Hierarchy unavailable; screenshot retained" > "$ARTIFACT_DIR/${ARTEMIS_RUN_ID}-hierarchy-unavailable.txt"
printf '%s\n' "$POLL_RESULT" > "$ARTIFACT_DIR/${ARTEMIS_RUN_ID}-poll-result.txt"
adb -s "$ANDROID_SERIAL" shell rm "$REMOTE_XML"
rm -f "$TMP_UI_XML"
```

Run the source `screencap` after the separate preparation attempt succeeds and before navigation starts. Run the landing `screencap` immediately after the navigation attempt exits, before the hierarchy poll or any scroll. Then use the bounded evidence scroll defined above. If the remote path already exists, choose a new run identifier; do not overwrite it. Record cleanup. Set `ISSUE_IDENTIFIER` and `ISSUE_TITLE` to the expected values from the case row before polling. The identifier match only synchronizes collection; it is not an assertion. These whole-document matches are diagnostic only and cannot pass a case:

```sh
grep -Fq -- "$ISSUE_IDENTIFIER" "$UI_XML" && grep -Fq -- "$ISSUE_TITLE" "$UI_XML" && ! grep -Fq -- 'does not exist' "$UI_XML" && ! grep -Fq -- 'issue not found' "$UI_XML"
```

Record the command and exit status as diagnostic evidence. A generic hierarchy node can contain these strings while navigation failed, so the grep result is never the case assertion. The screenshot oracle above controls PASS/FAIL.

## Comparison and qualification sequence

1. Install the pre-fix APK with `adb install -r`. Do not uninstall between builds. If Android reports `INSTALL_FAILED_VERSION_DOWNGRADE`, or package/signature mismatch prevents replacement, mark that install and dependent comparisons `NOT-RUN`; do not clear app data or change the authenticated account. Before the Case A pre-fix attempt, read only the existing `metadata.decision_park` precondition and record whether it is still an object. If it is absent, changed, or cannot be verified at the attempt, mark the Case A pre-fix comparison `NOT-RUN`; do not create or edit metadata. Case B's pre-fix comparison remains `NOT-RUN` unless its existing regression precondition is present and verified; do not manufacture it. Capture the source-card screenshot, first post-tap screenshot, and hierarchy diagnostics for each eligible pre-fix attempt. The expected Case A pre-fix error is an observation, not a deliberate assertion negative control. An inert Comment link is a navigation `FAIL`, and its metadata-regression comparison is `NOT-RUN` because the issue route was not reached.
2. Install the fixed APK with `adb install -r`, without uninstalling. Run Cases A and B. Require the issue-render screenshot oracle to pass each eligible case; record href/comment identity and hierarchy only as diagnostic evidence. Capture source, first post-tap, and header/title screenshots plus hierarchy for each attempt.
3. Return to the same source card and repeat Case A from a clean app navigation state without clearing app data. Require the screenshot oracle to pass again and retain all three screenshots and hierarchy. This is a clean runner-stability rerun and representative pass for the API 35 x86_64 first-run class qualification. It is limited regression evidence because the app may return to cached issue data; it does not establish a new fetch or parse.
4. **Qualification blocker: pinned Artemis cannot currently demonstrate the required failing exit for an assertion failure.** At runner `cbf2fea84cbd47ea54053239679ab521b01d4d23`, `artemis/sdk/agent.py` logs `tests_failed > 0`, finalizes the task without an error, and ends the session as `completed`; `artemis/interfaces/cli/commands/run.py` does not inspect the returned assertion summary. A strict Checker assertion failure can therefore produce a successful CLI exit. A shell inversion or a wrapper that changes that exit does not prove Artemis reports a failing exit. Do not claim the API 35 x86_64 class is qualified until the pinned runner or an approved report-gating path provides an Artemis-run-linked failing exit and preserves the report and traces. Record this qualification requirement as `BLOCKED`, with the run/report identifiers and artifact paths if an attempt is later authorized. Do not alter the runner in this case.

The pre-fix comparison, fixed-build pass, clean rerun, and any future Artemis-reported controlled failure are separate attempts. Never change the testcase expectation to turn an unexpected result into a pass. If the accessibility hierarchy cannot expose the issue identifier and title, retain it as diagnostic evidence and use the screenshot oracle; missing or unreadable screenshot evidence is `BLOCKED`.

## Setup, synchronization, and cleanup

- Before every attempt, confirm the installed APK SHA-256 and app source pin in the report. Use separate Artemis preparation and navigation attempts. Immediately capture the first post-tap screenshot, then poll for up to 60 seconds by repeating `uiautomator dump` and pulling the hierarchy. Treat dump failure, including a null root node, as retryable. A missing identifier or timeout must not prevent screenshot review. Retain the last successfully collected hierarchy, or record it unavailable. Record timeout as navigation `FAIL`, with the first post-tap screenshot and last observed state. Separate expected pre-fix errors from fixed-build failures.
- Confirm the app has an authorized, already authenticated account after each `adb install -r`. Do not put credentials in the case, command history, logs, screenshots, or report. If authentication is unavailable, record `NOT-RUN`.
- Reset between attempts by returning to the same existing source chat/card and reopening the plain `Comment` link. Do not clear app data, reset or reboot the AVD, restart shared ADB, or modify account/workspace data.
- Target all device commands at the exact serial from the active grant. Use a unique, task-scoped temporary hierarchy path and remove that exact path after collection. Stop only processes started by this run and record cleanup and release evidence for Sol.
- Preserve Artemis run reports, commands, exit statuses, screenshots, hierarchy captures, retry outcomes, app APK hashes, runner/testcase pins, and cleanup evidence on CHE-3531. Redact unrelated issue/chat content before attaching artifacts.

## Result states

- Fixed-build case: `PASS` only when Luna's same-attempt visual review confirms the expected identifier in the native navigation header, exact title in `IssueHeaderCard`, and no not-found/load-error text. Use the initial and later screenshots together when scrolling is needed. The source-card screenshot must show the named source text and plain Comment link. Href and comment identity are diagnostic evidence.
- Pre-fix comparison: record the observed error separately; never label it as the deliberate assertion failure. If the Comment link is inert and the issue route is not reached, navigation is `FAIL` and the metadata parsing-regression comparison is `NOT-RUN`.
- Clean rerun: `PASS` only when the same fixed-build screenshot oracle passes again after the defined navigation reset.
- Artemis failure-path qualification: remains `BLOCKED` until an Artemis-run-linked controlled assertion failure has a failing exit and its report and traces are preserved. Shell inversion alone does not qualify the runner.
- Missing grant, missing source card, missing artifact, uncertain cleanup, or unavailable target: `NOT-RUN` or `BLOCKED`, never `PASS`.

## References

- PR #918 documents the `metadata.decision_park` parse failure and the two issue repros.
- `apps/mobile/app/(app)/[workspace]/issue/[id].tsx` renders the issue detail and loading/error states.
- `apps/mobile/components/issue/issue-header-card.tsx` renders the issue identifier and title locations checked by Luna's screenshot oracle.
- Android execution, artifact, qualification, and cleanup requirements are governed by the live Android device squad grant and policy.
