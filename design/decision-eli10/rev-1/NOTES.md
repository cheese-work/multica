## What changed

The card keeps its header (title, state chip, issue chip, source comment link). The ask body changes order and the working notes collapse.

- Order: Context, Decision, What is at stake, Options (what happens, pros, cons, cost or time), Recommendation, Default if no reply, then a collapsed `Details` block holding "Agent did", then one "Investigate in issue" line with the issue and source comment links. The line sits outside Details, so it stays visible while Details is collapsed.
- Styling (`packages/views/rich-content/rich-content.css`): `details` gets a 1 px `--surface-border` border and `--radius-md`; `summary` is at least 36 px high, `--muted-foreground`, medium weight, keeps the native disclosure marker, and shows a 2 px `--ring` outline on keyboard focus. When open, a hairline separates the summary from the body.
- No card code changes. `RichContent` already renders `<details>`; `rehype-sanitize` keeps it.

## Evidence

Captures are from a local X99 Vite page that renders the real `DecisionRequestCard` and `RichContent` with the real tokens, light mode, reduced motion. Not a full app run with a live server.

| File | Shows |
| --- | --- |
| `before-1440.png`, `before-390.png` | The stored CHE-1755 ask, comment `01a17b71-6181-790a-a210-4848ca0e24d6`, in today's card |
| `after-1440-collapsed.png`, `after-390-collapsed.png` | The same decision in the new shape, Details collapsed |
| `after-1440-open.png`, `after-390-open.png` | Same, Details open |

The new-shape ask is the plan's draft JSON for CHE-1755, run through `Ask.Content()`. Its time values are estimates for the screenshot only.

## Limits

- The "Issue" chip after "Investigate in issue" shows a fallback label because the capture page has no issue data. In the app it shows the issue identifier.
- Keyboard and screen-reader behaviour, dark mode and measured contrast are NOT-RUN.
- Mobile rendering is not covered here; D3 owns it.
- Text inside a collapsed Details block is not selectable until opened (relevant to CHE-2175 annotations).
