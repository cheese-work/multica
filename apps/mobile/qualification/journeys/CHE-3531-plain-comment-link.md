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
mkdir -p "$ARTIFACT_DIR"
pnpm --filter @multica/mobile exec expo config --type public --json > "$ARTIFACT_DIR/expo-public-config.json"
jq -r '.android.package // "<unset>"' "$ARTIFACT_DIR/expo-public-config.json" > "$ARTIFACT_DIR/expo-public-android-package.txt"
pnpm --filter @multica/mobile exec expo prebuild --clean --platform android --no-install
```

The Expo public-config command above is the observation for the generated package value. Preserve its JSON output and the extracted `android.package` value in the report; do not infer a package from the app name.

The source `apps/mobile/app.config.ts` does not define `android.package`. Record that fact and the public Expo config's generated package value before changing the generated project. If prebuild does not complete without an unpinned source change, mark the build `NOT-RUN`; do not edit app source. In generated `apps/mobile/android/app/build.gradle`, set the single `defaultConfig.applicationId` property to `ai.multica.mobile`; stop as `NOT-RUN` if the property is absent or ambiguous. Record generated `compileSdkVersion`, `targetSdkVersion`, and `buildToolsVersion` from Gradle; platform `35` is the installed API target, not an assertion that generated compile SDK resolves to 35. This generated-file edit is identical for both builds and is not a source change. From `apps/mobile/android`, run `./gradlew :app:assembleRelease -PreactNativeArchitectures=x86_64`. Verify the APK reports package `ai.multica.mobile` and ABI `x86_64`; record complete tool versions, source SHA, lockfile SHA, command, package ID, ABI, resolved SDK values, and APK SHA-256 for each build. Do not proceed if either build cannot use these exact inputs.

For later granted execution, verify TLS/HTTP reachability from the AVD to both origins, then use an unauthenticated API request to confirm the API route responds without logging authorization headers or response bodies. The API client concatenates the origin with `/api/...` paths. Confirm the pre-existing app session is authenticated in the authorized workspace without opening or recording credentials; if origins or the existing session are unavailable, record `NOT-RUN`. Device reachability and authenticated-app readiness are NOT-RUN for this candidate.

Install each APK with `adb -s "$ANDROID_SERIAL" install -r`. If Android reports `INSTALL_FAILED_VERSION_DOWNGRADE`, or the package/signature prevents replacement, mark that install and dependent comparison `NOT-RUN`. Do not uninstall, clear app data, or use a downgrade workaround.

## Cases

Use an already authenticated, authorized workspace account. Do not record credentials or unrelated personal data. Do not create, edit, answer, or delete issues, comments, decision requests, or chat messages.

Run each case from its named existing chat/DM decision card. The read-only decision records currently report both requests as `answered`, but do not expose the selected answer. Do not infer the answer or permission from that state. Do not tap any answer control. If safe preparation requires an answer action, card mutation, or any navigation tap other than opening the existing source conversation, mark that preparation and dependent navigation `NOT-RUN`. If the existing card is unavailable, record `NOT-RUN`; do not create a substitute card. For navigation, the named card must already be visible before the Artemis attempt starts. Tap only its plain `Comment` link. Do not tap an Issue chip, paste a URL, or navigate by search.

| Case | Source card | Exact Artemis goal | Expected issue and linked-comment evidence |
| --- | --- | --- | --- |
| A | Existing Cheese Work DM decision request `32839d38-d445-48c5-952c-4ac477bc24ba`, asking whether to accept the CHE-3357 replay/sample proposal. Its `ping_message_id` is `01a1a166-673d-79bc-a412-d7a557df410c`. The exact message body links its plain `Comment` to `/cheese-work/issues/01a1a0eb-718d-778e-9948-770bc5e04ad7#comment-01a1a165-1b88-769c-8137-bd8be641b61b`. | `From the currently visible existing decision card for request 32839d38-d445-48c5-952c-4ac477bc24ba, tap only the plain Comment link whose target ends in comment-01a1a165-1b88-769c-8137-bd8be641b61b. Wait until the issue screen settles. Make no data changes.` | Target issue UUID `01a1a0eb-718d-778e-9948-770bc5e04ad7`; `CHE-3357`, `feat(multica): replay ask-guard signals on 7 days and sample the residual gap`; linked timeline comment `01a1a165-1b88-769c-8137-bd8be641b61b`, identified by `Verdict: BLOCKED / NOT-RUN` and reviewer `x99-codex-terra`. |
| B | Existing Cheese Work DM decision card posted 4 Nov 2026, asking Cheese to accept the frozen CHE-1554 r5 revision 2 owner card. Decision request `9d7091c7-5e0b-48db-8ba3-7921f36d128d`; its `ping_message_id` is `01a1a493-3dd7-79c9-8c39-1ca017a18299`. The exact message body links its plain `Comment` to `/cheese-work/issues/01a12f17-2493-7dff-a202-cac3a562ff36#comment-01a1a48c-0e98-7177-bf6e-3fcbd96afb5c`. | `From the currently visible existing decision card for request 9d7091c7-5e0b-48db-8ba3-7921f36d128d, tap only the plain Comment link whose target ends in comment-01a1a48c-0e98-7177-bf6e-3fcbd96afb5c. Wait until the issue screen settles. Make no data changes.` | Target issue UUID `01a12f17-2493-7dff-a202-cac3a562ff36`; `CHE-1554`, `chore(instructions): require markdown links for issues, PRs and files in human-facing text`; linked timeline comment `01a1a48c-0e98-7177-bf6e-3fcbd96afb5c`, a reply by `x99-codex-sol` under parent `01a1a482-6fdf-7443-abc6-2cef14519f66`, identified by its opening `Verdict: ready for Cheese's owner run.` |

Set non-empty values for `ANDROID_SERIAL`, `ARTEMIS_BATCH_ID`, `CASE_ID`, `BUILD_ID`, `PREP_GOAL`, `NAV_GOAL`, `PREP_ATTEMPT_ID`, `NAV_ATTEMPT_ID`, `ISSUE_IDENTIFIER`, `ISSUE_TITLE`, and `ARTIFACT_DIR`. Use the exact granted serial and task evidence directory. The two attempt IDs must differ and all batch, case, build, attempt, run, test, artifact, and trace identifiers must be unique across cases, app builds, retries, and the clean rerun. The attempt-ID ledger and exclusive directory creation below reject reused IDs and existing artifact paths before Artemis starts. Bind both attempts in one report row. GNU coreutils `timeout` must be available; if it is absent, collection is `NOT-RUN`, never unbounded. Run the following shell blocks for an attempt in one persistent Bash process; do not execute each fence in a new shell because later blocks use earlier variables. Artemis preparation and navigation remain separate `--standalone` invocations. A standalone invocation is a separate runner process and does not guarantee that the device remains on the prepared card. Capture the source card before starting navigation, and confirm it is still visible when the navigation invocation begins. If it is not visible, do not recover with another navigation tap; mark navigation `NOT-RUN`. Run Artemis from the pinned checkout with authorized host configuration:

Position on the source card in a separate Artemis preparation attempt. For Case A, open the named DM request and stop on its decision card. For Case B, open the named 4 Nov 2026 DM card and stop on that card. Save the preparation trace and exit status separately. Capture a non-empty source screenshot only after the card is visible and before the navigation attempt. If preparation does not land on the specified card, record `NOT-RUN` and do not start navigation. Do not tap the Comment link during preparation. Read-only `multica decision get` currently reports both records as `answered`; the answer value is unavailable in those records. Do not infer a choice or permission from `answered`. If positioning would require touching an answer control or changing card data, stop with that attempt `NOT-RUN`.

Use this exact preparation goal for Case A: `Open the existing Cheese Work DM decision request that asks, "Accept CHE-3357 as done with S3 and wakeup-update attribution recorded as NOT-RUN?" Stop on that decision card. Do not tap any link and make no data changes.` Use this exact preparation goal for Case B: `Open the existing Cheese Work DM decision card that asks, "Do you accept the frozen r5 revision 2 owner card, its candidate and presentation tradeoff, the exact tested-binary compatibility scope, and r5-first ordering for one owner-only transaction?" Stop on that card posted 4 Nov 2026. Do not tap any link and make no data changes.` The navigation goals are the exact goals in the table.

```sh
set -euo pipefail
command -v timeout >/dev/null
mkdir -p "$ARTIFACT_DIR"
PREP_RUN_ID="${ARTEMIS_BATCH_ID}-${CASE_ID}-${BUILD_ID}-prep-${PREP_ATTEMPT_ID}"
PREP_TEST_NAME="${CASE_ID}-${BUILD_ID}-prep-${PREP_ATTEMPT_ID}"
PREP_ARTIFACT_DIR="$ARTIFACT_DIR/$PREP_RUN_ID"
PREP_TRACE_PATH="$PREP_ARTIFACT_DIR/traces"
NAV_RUN_ID="${ARTEMIS_BATCH_ID}-${CASE_ID}-${BUILD_ID}-nav-${NAV_ATTEMPT_ID}"
NAV_TEST_NAME="${CASE_ID}-${BUILD_ID}-nav-${NAV_ATTEMPT_ID}"
NAV_ARTIFACT_DIR="$ARTIFACT_DIR/$NAV_RUN_ID"
NAV_TRACE_PATH="$NAV_ARTIFACT_DIR/traces"
ATTEMPT_LEDGER="$ARTIFACT_DIR/CHE-3531-attempt-ids.txt"
touch "$ATTEMPT_LEDGER"
if [[ "$PREP_ATTEMPT_ID" == "$NAV_ATTEMPT_ID" \
  || -e "$PREP_ARTIFACT_DIR" \
  || -e "$NAV_ARTIFACT_DIR" \
  || $(grep -Fxc -- "$PREP_ATTEMPT_ID" "$ATTEMPT_LEDGER") -gt 0 \
  || $(grep -Fxc -- "$NAV_ATTEMPT_ID" "$ATTEMPT_LEDGER") -gt 0 ]]; then
  echo "Attempt ID or artifact path already exists; choose new unique IDs and paths" >&2
  exit 2
fi
mkdir "$PREP_ARTIFACT_DIR"
mkdir "$NAV_ARTIFACT_DIR"
printf '%s\n%s\n' "$PREP_ATTEMPT_ID" "$NAV_ATTEMPT_ID" >> "$ATTEMPT_LEDGER"
```

```sh
set +e
uv run artemis run "$PREP_GOAL" --standalone --profile pro --verification-level strict --device-serial "$ANDROID_SERIAL" --test-name "$PREP_TEST_NAME" --run-id "$PREP_RUN_ID" --traces-path "$PREP_TRACE_PATH"
PREP_EXIT=$?
set -e
printf '%s\n' "$PREP_EXIT" > "$PREP_ARTIFACT_DIR/${PREP_RUN_ID}-exit-status.txt"
test "$PREP_EXIT" -eq 0
```

After the preparation attempt succeeds and its trace/exit status are saved, confirm the named decision card is visible. Capture its source screenshot before starting navigation:

```sh
SOURCE_SCREENSHOT="$PREP_ARTIFACT_DIR/${PREP_RUN_ID}-source.png"
adb -s "$ANDROID_SERIAL" exec-out screencap -p > "$SOURCE_SCREENSHOT"
test -s "$SOURCE_SCREENSHOT"
```

Standalone mode runs a separate Artemis process with verification, test name, run ID, and trace flags. It does not guarantee app-state persistence between invocations; the device may retain its visible app state, but this must be confirmed on-screen before navigation starts. If the named card is no longer visible, do not add recovery taps and record navigation `NOT-RUN`. If Artemis routes to or starts the shared daemon, stop and record the attempt as `FAIL`; do not stop a pre-existing shared daemon. Record process exit status, Artemis task/report identifiers, runner checkout SHA, configured model ID, and generated trace paths. Do not include API keys. Do not use model narration as the UI assertion.

The decision records' `ask` fields returned through `multica decision get` are bound to the exact source messages by matching `ping_message_id`; those bodies establish the hrefs above. Do not substitute `issue_comment_id`, `source_comment_id`, a nearby reply, or an answer comment for the href target. Confirm the resulting issue UUID and exact title against the selected case row. A mismatch is `FAIL`; do not continue that case.

For every navigation attempt, the non-empty source-card screenshot must show enough of the named decision text and plain `Comment` link to identify the source card. Redact unrelated chat content. Start a separate Artemis navigation attempt only while that prepared card is currently visible. Give it its own unique run ID, test name, artifact directory, and trace path. Its goal begins from the visible card, taps only the named plain `Comment` link, and stops after the first post-tap screen settles; it does not scroll. Immediately after Artemis exits, capture and preserve the first post-tap screen before any hierarchy poll, scroll, or other inspection. A missing issue identifier or hierarchy dump failure must never skip the screenshot. Then collect hierarchy diagnostically, retaining the last successfully pulled non-empty XML. Poll for at most 60 seconds including the bounded duration of every hierarchy subprocess. Record hierarchy availability, subprocess timeout, deadline expiration, identifier observation, and visual navigation result as separate values. A hierarchy identifier may match the source chat and is not route evidence. Hierarchy timeout or missing identifiers do not determine navigation PASS/FAIL. Luna reviews the preserved screenshots: correct native header identifier, exact title, and no not-found/load-error text establish visual PASS; a visible wrong page, not-found/load error, or inert link establishes navigation FAIL. If screenshots cannot establish either outcome, record `BLOCKED`/inconclusive. Keep expected pre-fix error observations separate from fixed-build failures.

The visual oracle for mobile acceptance is the native issue page render: the native navigation header shows the expected identifier, `IssueHeaderCard` shows the exact title, and the screen has no not-found/load-error text. The initial post-tap screenshot and a later header screenshot together may prove these checks. Do not require automatic scrolling to the linked comment for issue-render PASS. Record the href and expected comment identity as diagnostic evidence. A whole-hierarchy identifier match may come from the source chat and is not route evidence. A wrong issue identifier/title or an inert plain Comment link is `FAIL`; if the issue route was not reached, the metadata parsing-regression comparison is `NOT-RUN`. Do not use an Issue chip, search, pasted URL, or edited card.

If the exact title is not visible in the initial screenshot, inspect the same issue screen only after preserving that screenshot. Read `wm size` on the granted serial. Use the `Override size` dimensions when present; otherwise use the `Physical size`. Record the complete output and the selected width and height. Calculate `X=width/2`, `Ystart=floor(height*0.25)`, and `Yend=floor(height*0.75)`. Issue at most 40 downward `adb -s "$ANDROID_SERIAL" shell input swipe "$X" "$Ystart" "$X" "$Yend" 500` gestures, one at a time, capturing a non-empty screenshot after each. Stop when the native header identifier and full exact title are visible, or when two consecutive screenshots show no visible movement in the timeline content region below the native toolbar and above the system navigation area (ignore the status-bar clock), or the 40-gesture cap is reached. Do not tap during this evidence scroll. A downward gesture can trigger pull-to-refresh/refetch at the top; record any visible refresh indicator and refetch, and keep the initial post-tap screenshot as the primary landing evidence. Later inspection screenshots are supplemental and cannot replace or rewrite the initial landing result. Record every gesture and screenshot filename. If the title is still not visible at the stop condition, record the visual oracle as `BLOCKED`/inconclusive, not `FAIL`; an inspection bound is not evidence of a wrong issue page. Bind source-preparation trace/screenshot, navigation trace, initial post-tap screenshot, each evidence-scroll screenshot, later header screenshot, hierarchy diagnostics, source message ID, href target comment ID, issue UUID, app pin, and both distinct Artemis attempt IDs in one report row.

For Case A, record diagnostic comment identity `01a1a165-1b88-769c-8137-bd8be641b61b`, opening `Verdict: BLOCKED / NOT-RUN`, by `x99-codex-terra`. For Case B, record diagnostic identity `01a1a48c-0e98-7177-bf6e-3fcbd96afb5c`, opening `Verdict: ready for Cheese's owner run.`, by `x99-codex-sol`, as a reply under `01a1a482-6fdf-7443-abc6-2cef14519f66`. Comment identity is diagnostic and does not gate the narrow issue-rendering PASS. Sol signs the returned execution evidence when verifying the run. Capture only the target issue content needed for this check.

## Sonnet advisory dispositions

- Finding 4, pull-to-refresh: a downward evidence swipe may trigger `RefreshControl` and refetch. The first post-tap screenshot remains the primary landing evidence. Record any visible refresh indicator and refetch; later inspection screenshots are supplemental and cannot replace the landing screenshot.
- Finding 5, source-chat identifier: a whole-document hierarchy identifier can match the source DM. It is diagnostic synchronization only and cannot establish that the issue route was reached.
- Finding 6, movement comparison: compare the content region below the native toolbar and above system navigation. Ignore status-bar clock changes. Stop after two consecutive screenshots show no visible timeline movement there.
- Finding 7, variables and shell scope: list every required variable before the blocks. Run the blocks for an attempt in one persistent Bash process so variables persist; Artemis preparation and navigation remain separate `--standalone` process invocations.
- Finding 8, captures and dimensions: require non-empty source, landing, and per-gesture screenshots. Select `Override size` from `wm size` when present; otherwise use `Physical size`, and record the output and selected dimensions.
- Finding 9, answer controls: read-only records report both decision requests as `answered`, but the selected answer is not exposed. Do not infer permission or answer either request. If safe positioning would require touching an answer control, stop and mark the preparation and dependent navigation `NOT-RUN`.

## Independent UI assertion

Artemis navigation and its final report are not the assertion. Use separate preparation and navigation attempts. Capture the source-card screenshot after preparation and before navigation. Immediately after the navigation attempt exits, capture the first post-tap screen before hierarchy collection or scrolling. Luna's same-attempt screenshot review checks the native header identifier, exact title in `IssueHeaderCard`, and absence of not-found/load-error text. The hierarchy is diagnostic only. Inspect the navigation trace and confirm it tapped only the named plain `Comment` link. Record evidence-scroll gestures outside the navigation trace. Any other Artemis tap or route change within the navigation attempt is `FAIL`; preparation navigation is recorded only in its separate preparation trace. Set `ANDROID_SERIAL`, `ARTEMIS_BATCH_ID`, `CASE_ID`, `BUILD_ID`, `PREP_GOAL`, `NAV_GOAL`, distinct `PREP_ATTEMPT_ID` and `NAV_ATTEMPT_ID`, `ISSUE_IDENTIFIER`, `ISSUE_TITLE`, and `ARTIFACT_DIR` before the one-shell attempt blocks. Use unique local and remote artifact names for each capture and a unique temporary XML path on the device.

```sh
set -euo pipefail
test -d "$NAV_ARTIFACT_DIR"
test ! -e "$NAV_ARTIFACT_DIR/${NAV_RUN_ID}-exit-status.txt"
```

Run the navigation attempt from the prepared source card:

```sh
set +e
uv run artemis run "$NAV_GOAL" --standalone --profile pro --verification-level strict --device-serial "$ANDROID_SERIAL" --test-name "$NAV_TEST_NAME" --run-id "$NAV_RUN_ID" --traces-path "$NAV_TRACE_PATH"
NAV_EXIT=$?
set -e
printf '%s\n' "$NAV_EXIT" > "$NAV_ARTIFACT_DIR/${NAV_RUN_ID}-exit-status.txt"
```

Immediately after Artemis exits, preserve the first post-tap screen. Then run the hierarchy collection block. Its values remain diagnostic and do not decide the visual result:

```sh
set -euo pipefail
UI_XML="$NAV_ARTIFACT_DIR/${NAV_RUN_ID}-landing.xml"
TMP_UI_XML="$NAV_ARTIFACT_DIR/${NAV_RUN_ID}-landing.tmp.xml"
LANDING_SCREENSHOT="$NAV_ARTIFACT_DIR/${NAV_RUN_ID}-landing.png"
REMOTE_XML="/sdcard/che-3531-${NAV_RUN_ID}.xml"
COLLECTION_STATE="$NAV_ARTIFACT_DIR/${NAV_RUN_ID}-collection-state.txt"
HIERARCHY_STATUS=unavailable
HIERARCHY_DEADLINE=not-expired
HIERARCHY_IDENTIFIER=not-observed
HIERARCHY_SUBPROCESS_TIMEOUT=not-observed
VISUAL_NAVIGATION_RESULT=not-assessed
LANDING_SCREENSHOT_STATUS=not-captured
REMOTE_XML_CLEANUP=not-needed
TMP_XML_CLEANUP=not-needed
REMOTE_XML_CREATED=0
write_collection_state() {
  printf 'hierarchy_status=%s\nhierarchy_deadline=%s\nhierarchy_subprocess_timeout=%s\nhierarchy_identifier=%s\nvisual_navigation_result=%s\nlanding_screenshot=%s\nremote_xml_cleanup=%s\ntemporary_xml_cleanup=%s\n' \
    "$HIERARCHY_STATUS" "$HIERARCHY_DEADLINE" "$HIERARCHY_SUBPROCESS_TIMEOUT" \
    "$HIERARCHY_IDENTIFIER" "$VISUAL_NAVIGATION_RESULT" "$LANDING_SCREENSHOT_STATUS" \
    "$REMOTE_XML_CLEANUP" "$TMP_XML_CLEANUP" > "$COLLECTION_STATE"
}
cleanup_collection() {
  if (( REMOTE_XML_CREATED )); then
    if timeout --signal=TERM --kill-after=1s 5s adb -s "$ANDROID_SERIAL" shell rm -f "$REMOTE_XML" >/dev/null 2>&1; then
      REMOTE_XML_CREATED=0
      REMOTE_XML_CLEANUP=removed
    else
      REMOTE_XML_CLEANUP=unconfirmed
    fi
  fi
  if [[ -e "$TMP_UI_XML" ]]; then
    if rm -f -- "$TMP_UI_XML"; then
      TMP_XML_CLEANUP=removed
    else
      TMP_XML_CLEANUP=unconfirmed
    fi
  fi
  write_collection_state
}
trap cleanup_collection EXIT
trap 'HIERARCHY_DEADLINE=interrupted; exit 130' INT TERM
# Preserve the first post-tap screen before diagnostics, including error pages.
if adb -s "$ANDROID_SERIAL" exec-out screencap -p > "$LANDING_SCREENSHOT" && test -s "$LANDING_SCREENSHOT"; then
  LANDING_SCREENSHOT_STATUS=non-empty
else
  LANDING_SCREENSHOT_STATUS=missing-or-empty
  exit 1
fi
: "${ISSUE_IDENTIFIER:?set the expected issue identifier before collection}"
: "${ISSUE_TITLE:?set the expected exact title before collection}"
if ! timeout --signal=TERM --kill-after=1s 5s adb -s "$ANDROID_SERIAL" shell test ! -e "$REMOTE_XML"; then
  REMOTE_XML_CLEANUP=path-collision
  exit 2
fi
COLLECTION_DEADLINE=$((SECONDS + 60))
run_bounded_adb() {
  local remaining=$((COLLECTION_DEADLINE - SECONDS))
  local command_seconds=15
  (( remaining > 2 )) || return 124
  if (( command_seconds > remaining - 2 )); then
    command_seconds=$((remaining - 2))
  fi
  timeout --signal=TERM --kill-after=1s "${command_seconds}s" adb -s "$ANDROID_SERIAL" "$@"
}
while (( SECONDS < COLLECTION_DEADLINE )); do
  REMOTE_XML_CREATED=1
  if run_bounded_adb shell uiautomator dump "$REMOTE_XML" >/dev/null 2>&1; then
    if run_bounded_adb exec-out cat "$REMOTE_XML" > "$TMP_UI_XML" && test -s "$TMP_UI_XML"; then
      cp "$TMP_UI_XML" "$UI_XML"
      HIERARCHY_STATUS=available
      if grep -Fq -- "$ISSUE_IDENTIFIER" "$UI_XML"; then
        HIERARCHY_IDENTIFIER=observed
        break
      fi
    else
      command_status=$?
      (( command_status == 124 )) && HIERARCHY_SUBPROCESS_TIMEOUT=observed
    fi
  else
    command_status=$?
    (( command_status == 124 )) && HIERARCHY_SUBPROCESS_TIMEOUT=observed
  fi
  remaining=$((COLLECTION_DEADLINE - SECONDS))
  (( remaining > 0 )) || break
  sleep "$((remaining > 1 ? 1 : remaining))"
done
(( SECONDS >= COLLECTION_DEADLINE )) && HIERARCHY_DEADLINE=expired
if [[ "$HIERARCHY_STATUS" == unavailable ]]; then
  echo "Hierarchy unavailable; landing screenshot retained" > "$NAV_ARTIFACT_DIR/${NAV_RUN_ID}-hierarchy-unavailable.txt"
fi
write_collection_state
```

Run the source `screencap` after the separate preparation attempt succeeds and before navigation starts. Require a non-empty source capture. Run the landing `screencap` immediately after the navigation attempt exits, before the hierarchy poll or any scroll, and require a non-empty file. If capture fails or is empty, preserve the file and collection-state report and mark the visual result `BLOCKED`; do not recapture over the primary landing file. Then use the bounded evidence scroll defined above. If the remote path already exists, choose a new navigation run identifier before execution; do not overwrite it. The 60-second collection deadline includes each bounded `uiautomator dump` and `exec-out cat`, with a two-second reserve for timeout termination. The separate cleanup is bounded to five seconds and removes only this attempt's remote path. The `EXIT`, `INT`, and `TERM` paths retain the last non-empty XML, write collection state, and attempt scoped cleanup. `remote_xml_cleanup=unconfirmed` requires Sol recovery and is not a clean attempt. Set non-empty `ISSUE_IDENTIFIER` and `ISSUE_TITLE` values from the case row before running the poll block; the `:?` guards stop collection if either is empty. An identifier match may come from the source chat; it only synchronizes collection and is never evidence that the issue route was reached. These whole-document matches are diagnostic only and cannot pass a case:

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

- Before every attempt, confirm the installed APK SHA-256 and app source pin in the report. Use separate Artemis preparation and navigation attempts with distinct run IDs, test names, artifact subdirectories, and trace paths. Immediately capture the first post-tap screenshot, then poll for up to 60 seconds by repeating `uiautomator dump` and pulling the hierarchy. Treat dump failure, including a null root node, as retryable. A missing identifier or timeout must not prevent screenshot review. Retain the last successfully collected non-empty hierarchy, or record it unavailable. Record hierarchy availability, deadline expiration, and identifier observation separately; timeout alone does not determine navigation result. The same-attempt visual review establishes `PASS`, `FAIL`, or `BLOCKED`/inconclusive. Separate expected pre-fix errors from fixed-build failures.
- Confirm the app has an authorized, already authenticated account after each `adb install -r`. Do not put credentials in the case, command history, logs, screenshots, or report. If authentication is unavailable, record `NOT-RUN`.
- Before each new attempt, use a separate preparation run to position on the same existing source chat/card. Capture and retain its source screenshot and preparation trace. The navigation run begins only if the card is already visible and its trace taps only the plain `Comment` link. Do not clear app data, reset or reboot the AVD, restart shared ADB, or modify account/workspace data.
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
