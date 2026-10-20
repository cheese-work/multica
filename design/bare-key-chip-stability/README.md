## Request

Reported by Cheese on 16 October 2026 (DM with x99-claude-opus, about 23:30 ICT) and recorded on CHE-1827: the web client, also in a mobile browser, "flickers a lot when a bare issue key such as `CHE-1824` renders as a rich chip". Cheese chose the full fix (D1 = B) at 23:50 ICT the same day. That decision includes a loading box and a resolved chip of the same size, so the key does not move the text after it.

The coupled-width fix makes a resolved bare-key chip as wide as its loading box. A short-title chip grows from 138 px to 288 px. rev-1 shows that look (A) and one smaller alternative (B). Luna scored both directions before Cheese saw them.

- Issue: [CHE-1827](mention://issue/01a14594-3b86-77d5-b2b1-45415e7e761a), slice CHE-1829
- Pages in scope: the bare-key presentation only (an issue key typed in a chat message or an issue description). `packages/views/issues/components/issue-chip.tsx`, `issue-chip-loading.tsx`, `packages/views/rich-content/rich-content.tsx`. Explicit authored URL chips and UUID mention chips stay content-sized.
- Tokens: `packages/ui/styles/tokens.css`
- Status: selected rev-1 direction B — approved when PR #294 merges

## Selection

Cheese selected direction B of rev-1 (14 rem, 224 px). Source: CHE-1827 comment `01a15750-f3e6-74f1-9675-771347dc32f2`, a native quote of Cheese's DM reply of 20 October 2026, about 10:20 ICT. Its item 2 reads:

> 2. B

Item 1 of that quote concerns another issue. A chat selection is not approval. Cheese approves by merging PR #294, or records a scoped waiver that names the design-admission requirement. The scored revision is `rev-1` at commit `849203d51f285ed2807d1f75cf0890d32e5448a4` (UX verdict: CHE-1827 comment `01a14644-4e3c-72ec-ab93-25af3fc90e08`). That commit is the revision commit in the `DESIGN.md` row. The commit that records this selection is a later commit on the same branch and adds no revision.

## Open: the B mobile wrapping mismatch

`rev-1/B-390.png` wraps "after." onto a second line. The product-harness capture `rev-1/product-crosscheck/B-14rem-mobile-short-2-resolved.png` keeps "now and after." on one line. The cause is the width the prose has, not the cap. See `analysis/b-wrap-mismatch.md`. B is not promised to keep prose on one line at 390 px.
