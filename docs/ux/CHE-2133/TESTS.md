## Test-first receipt

Host: congvc-X99. Parent: `84afc806a3e4bf8ae474f788defafa3afaf68a43`.
Test: `packages/views/issues/components/comment-card-own.test.tsx`.

The test was written and run before production code changed. The observed red run began on 27 October 2026 at 12:17 ICT.
An earlier invalid collapse-store setup run is excluded from behavioral red evidence.

Command for both the observed red and focused green:

```bash
pnpm --filter @multica/views exec vitest run issues/components/comment-card-own.test.tsx --maxWorkers=2
```

Observed red output, excerpt:

```text
FAIL  issues/components/comment-card-own.test.tsx > viewer-authored issue comments > fills only the viewer's member comment in the root position
Error: expect(element).toHaveClass("comment-own")

Expected the element to have class:
  comment-own
Received:
  transition-colors duration-700

Test Files  1 failed (1)
     Tests  11 failed | 2 passed (13)
```

The reply-position ownership test failed on the same missing class. Locale tests failed because the You labels were absent.
The negative signed-out case and deleted-placeholder case passed on the parent.
During green verification, the collapse assertion was corrected to check the body region, not its existing collapsed header preview.
No ownership assertion was changed after the observed red run.

Observed focused green output, 27 October 2026 at 12:39 ICT:

```text
Test Files  1 passed (1)
     Tests  13 passed (13)
```

## Regression checks

The following command passed with 4 files and 25 tests on 27 October 2026 at 12:40 ICT:

```bash
pnpm --filter @multica/views exec vitest run issues/components/comment-card-deleted.test.tsx issues/components/comment-card-edit-gate.test.tsx issues/components/comment-card-copy-link.test.tsx rich-content/self-mention.test.tsx --maxWorkers=2
```

Total focused and regression coverage: 38 passing tests across 5 files.
Coverage includes mixed actors, root/reply ownership, target precedence, all five locales, deleted placeholders, collapsed and editing headers, resolved/folded replies, copy links and self-mentions.

```bash
pnpm --filter @multica/views exec eslint issues/components/comment-card.tsx issues/components/comment-card-own.test.tsx
pnpm --filter @multica/views exec tsc --noEmit
git diff --check
```

Full red, green and regression logs are in the evidence archive attached to the issue.
