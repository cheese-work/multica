# UI performance scenarios

The comment-typing scenario models a long issue thread while three agent runs
stream progress. It exists because of MUL-7227, where a summary animation
replaced a DOM node per streamed message and typing a comment slowed to a crawl.
Component tests could not see it — they run in jsdom, which has no style
recalculation, no layout and no main-thread contention.

## What it runs against

The real Next.js production build, the real page, the real shell and CSS, the
real ProseMirror composer. Only HTTP and the WebSocket are synthetic, so there
is no API server, database, daemon, agent or account involved. Nothing disables
animations or trims the DOM: the cost being measured lives in that machinery.

It covers the renderer shared by Web and Desktop. Electron's own main-process
behaviour is not exercised here.

## Running it

Against a frontend you already have running:

```bash
pnpm exec playwright test --config=playwright.perf.config.ts
# PLAYWRIGHT_BASE_URL defaults to http://localhost:3000
# PERF_REPORT_PATH=<file> also writes the JSON report
```

Comparing two refs — this builds and measures each one, in sequence:

```bash
node scripts/perf-compare.mjs --base origin/main --head HEAD --out perf-report
```

The comparison needs Node, pnpm and Chromium. It writes `comparison.json` and
`comparison.md`. Each side stops its server, waits for it to exit and removes
its worktree before the next side starts, and the script exits on its own on
success, on failure and on cancellation.

The ordinary e2e suite ignores this directory; it needs a production build and
a machine that is not busy doing anything else.

The floating-chat lifetime regression runs against the same production build:

```bash
pnpm exec playwright test --config=playwright.perf.config.ts floating-chat-lifetime.spec.ts
```

It checks that closing removes the message list, keeps the real composer and
an in-flight upload alive, shows messages received while closed on reopen, and
survives an interrupted exit. It also checks that the message list's ancestor
is never scaled, and that a reopened window never paints before its message
list does. HTTP and WebSocket traffic is synthetic; no backend is needed.

## Layout-stability profiles

`chip-stability.spec.ts` (issue-key chips while streaming and while resolving)
runs once per profile project, both under 4x CPU throttling:

```bash
pnpm exec playwright test --config=playwright.perf.config.ts chip-stability            # both
pnpm exec playwright test --config=playwright.perf.config.ts chip-stability --project=phone
```

`desktop` is Chromium 1440x900 without touch; `phone` is `devices['Pixel 7']`
(touch, mobile). Geometry limits are shared; layout-shift limits are keyed by
profile in the spec. Soft assertions: one run reports every requirement.

`compact-thread-reply.spec.ts` (R5, CHE-1935) opens a compact thread, focuses
the reply box (click on `desktop`, `tap` on `phone`) and types one character.
It asserts the fold state, reply-box top and viewport scroll are unchanged
(<= 1 px) and gates the raw layout-shift sum, because standard CLS drops
shifts within 500 ms of input.

## Page-load baseline (CHE-2976)

Cold load of five routes (issue list, issue detail, chat, inbox, projects) on
two fixed profiles, one Playwright project each: `desktop` (1440×900, no touch)
and `phone` (`devices["Pixel 7"]`, touch). Both throttle the CPU 4x; `phone`
also emulates 150 ms RTT and 1.6 Mbps down. Each sample is a fresh browser
context: TTFB, LCP, total long-task time and JS transferred, 5 samples, median.

```bash
pnpm exec turbo build --filter=@multica/web
REMOTE_API_URL=http://127.0.0.1:1 PORT=3931 pnpm --filter @multica/web start &
for p in desktop phone; do
  PERF_PROFILE=1 PERF_REPORT_DIR=perf-report PLAYWRIGHT_BASE_URL=http://127.0.0.1:3931 \
    pnpm exec playwright test --config=playwright.perf.config.ts page-load-baseline --project=$p
done
node scripts/page-load-report.mjs perf-report/page-load-desktop.json perf-report/page-load-phone.json
```

`PERF_SAMPLES` changes the sample count. `PERF_PROFILE=1` adds one extra load per
route under the CPU profiler; its time-by-file evidence is written to the JSON
and never joins the medians. A sample whose page never showed its data, or that
made an unmocked request or threw a page error, fails the run instead of
reading as fast. The phone run takes about 10 minutes.

Phone network: Chromium throttles the document body at 1.6 Mbps but adds no
latency, so the spec adds one RTT. A phone sample fails if its document arrived
faster than the bandwidth allows. Synthetic API responses wait RTT plus their
transfer time; they do not share bandwidth with other requests.

## Scroll stability (CHE-3231)

Measures how far the row a reader is looking at moves unasked, in the chat list
and the issue comment timeline. Four scenarios in `scroll-stability.spec.ts`:
C1 (a new message moves the page boundary), C2 (ten run-start events for
unrelated issues in 2 s, no new message), C3 (both) and D1 (scroll up three
screens through 45 comments with code blocks).

```bash
pnpm exec turbo build --filter=@multica/web
REMOTE_API_URL=http://127.0.0.1:1 PORT=3931 pnpm --filter @multica/web start &
PLAYWRIGHT_BASE_URL=http://127.0.0.1:3931 pnpm exec playwright test \
  --config=playwright.perf.config.ts scroll-stability.spec
```

Report-only by default: each scenario prints `SCROLL_STABILITY_REPORT {json}`
with `jump_count` (frame steps where the anchor moved more than 1 px unasked),
`max_delta_px`, `lost_frames` and, for chat, `chat_requests` (message-list
requests for the DM). `PERF_REPORT_DIR` also writes one JSON per scenario.
`SCROLL_STABILITY_ASSERT=1` makes a scenario assert jump count 0, max delta
<= 1 px and, for C2, zero requests for the DM. A fix slice sets it; CI does
not until the CHE-2986 gating PR.

`lib/scroll-anchor.ts` is the meter; `scroll-anchor-helper.spec.ts` tests it
against a synthetic list that does or does not insert content above the anchor.
That self-test needs no server.

## When to run it

Nothing runs this automatically. The routine guard is the component test from
#8260, which pins the exact failure mode behind MUL-7227 — a streamed message
replacing the summary's DOM node — and runs with the normal suite.

Run the comparison yourself, or through the **UI performance report** workflow
(`workflow_dispatch`, base and head refs as inputs), when a change touches
summary animation, long lists, transcript rendering or global styles — and
attach the report to the PR. The cost of doing it by hand instead of on every
PR is that a regression outside those areas will not be caught automatically.

## What it costs

On a GitHub-hosted `ubuntu-latest` runner the comparison took 340.5 s: 291.5 s
for the two production builds, 26 s for the two measurements, the rest install
and startup — plus checkout, dependency and Chromium setup around it. The job is
capped at 15 minutes. Locally, a repeat comparison whose builds come out of the
turbo cache takes under a minute; the report says when a build was restored.

## Reading the result

`typing_elapsed_ms` is the primary number: how long the user waits from the
first keystroke until the editor holds the whole string, main-thread queueing
included. `recalc_style_ms`, `layout_ms`, `task_ms` and the long-task counters
say where the time went.

Two things are not the same as a fast run, and the scenario reports them as
`invalid` or `timeout` rather than as a duration: messages that never reached
the UI, and a composer that did not end up holding every character. An empty
page types very quickly.

One sample per ref. Read a small difference as noise until a second run says
otherwise. Locally the spread across three runs of one build was around 1%; on
CI, comparing two refs with no product difference between them came out 2%
apart on typing and on style recalculation. A threshold needs more CI samples
than that before it can be trusted.

## Changing the fixture

`fixtures/typing-under-live-runs.ts` generates every byte the scenario feeds the
product, and the report carries the fixture version and a digest of it. Bump
`FIXTURE_VERSION` whenever the generated bytes change — two runs are only
comparable when the digests match, which the comparison report states.

Do not add DOM to the page to keep the pressure up. A future virtualization
change that legitimately renders fewer nodes should show up as fewer nodes.
