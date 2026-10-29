## Approved design

Direction C, revision 1: `design/own-comment-background/rev-1/C.html`.
Cheese approved the design by merging PR #405 at `0c28149a888c902d350cb27560743b89623b7ce6`.
The scored source revision is `87dc762d3f23755f66dbc532156e9294e65d156e`.
Admission sheet: `../CHE-2133.yml`.

## Capture provenance

Captured on congvc-X99 on 27 October 2026 (ICT), using Chrome 150.0.7871.181, Vite, and the real shared `CommentCard`.
The fixture uses UI Lab's synthetic API, auth, workspace and navigation providers.
It does not connect to production or contain real issue data.

Before: the capture server loads `comment-card.tsx` directly from `84afc806a3e4bf8ae474f788defafa3afaf68a43` with a Vite source-load override.
After: the same server loads the working-tree component, whose SHA-256 is `b50df5424ed4219263d45f659f54b70df501090fa9063273f646952f6e33e7fe`.
The fixture, providers, data and viewport sizes are identical between before and after.
The new locale keys have no effect on the before component, which does not request them.
The fixture and capture script are delivered in the issue's evidence archive, not shipped in the application.

## UI review pack

- Preview: not available. The task-local browser and Vite server are stopped after capture.
- Visual diff: NOT-RUN (CHE-1485 Layer B pending).
- Widths: 1440 desktop and 390 responsive web. Device scale factor: 1.
- Long thread: 13 visible comment blocks, including a deleted root placeholder and a folded-resolution thread.

| Theme | Width | Before | After | Scrolled own header |
| --- | --- | --- | --- | --- |
| Light | 1440 | `thread-light-1440-before.png` | `thread-light-1440-after.png` | `sticky-light-1440.png` |
| Light | 390 | `thread-light-390-before.png` | `thread-light-390-after.png` | `sticky-light-390.png` |
| Dark | 1440 | `thread-dark-1440-before.png` | `thread-dark-1440-after.png` | `sticky-dark-1440.png` |
| Dark | 390 | `thread-dark-390-before.png` | `thread-dark-390-after.png` | `sticky-dark-390.png` |

## Manual Keep / Change / Remove check

- Keep: `comment-card` remains present. Author, timestamp, actions, code, table, Mermaid frame, replies, composer, resolved fold and deleted placeholder remain present.
- Change: viewer member roots and replies have the 8% light / 15% dark brand fill and a translated You label. Other members and agents stay neutral, including an agent with the viewer's ID.
- Change: sticky own headers use the exact same computed background as their comment. Browser scroll checks place the header at top 0 within its own comment.
- Change: jump targets keep the existing 5% brand fill, which takes precedence over the own fill. A pointer-transparent 2px brand outline distinguishes targets without relying on two nearly identical fills. The You label remains on own targets.
- Removed: none. The fill ends at the comment's boundary, not at the end of the thread.

`capture-evidence.json` records computed backgrounds, counts, target border, unchanged self-mention markup, no page-width overflow and sticky positions for every capture.
Before has zero own classes and You labels. After has four non-target own blocks and five You labels, including the own target.
All 12 PNGs were inspected on a contact sheet. Phone after and pinned-header captures were also opened at full size.

## Limits and reconciled baseline

These are actual component captures with local synthetic data, not authenticated production-page captures or native Electron-window captures.
Native mobile owns separate React Native views and is not implemented or verified by this shared web/desktop change.
Screen-reader execution is NOT-RUN. The You text is ordinary accessible text, not a colour-only ownership cue.
Headless full-page phone capture does not paint the offscreen sandboxed Mermaid iframe. The scrolled PNGs show the actual rendered diagram.
The first cold baseline can reserve extra Mermaid height. This PR does not change the renderer.

The approved mock describes the older amber self-mention. Main subsequently changed it to solid brand in CHE-2167 (`f5c2aab81`).
This PR leaves that newer self-mention implementation and CSS untouched. Existing CHE-2113 identity behavior and self-mention regression tests remain covered.

## Delivery gates

The implementation PR stays draft until checks pass and the leader-admitted different-lab signing reviewer returns an exact-head verdict.
Design approval does not authorize implementation merge or C00 deployment. Cheese merges. CHE-2112 owns deployment order.
