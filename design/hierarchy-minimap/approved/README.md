Locked from doop file `CHE-2749 · hierarchy-minimap (previs)` (https://congvc-c00.taila6fa8a.ts.net:34443/c/1AmQcGCI1A), page `1AmQcGCI1A:page1`. Issue [CHE-2749](mention://issue/01a1926a-aada-7893-8cba-33d866aca2f1).
Direction A (Tree) selected by Cheese (`01a19285-0f08-7cbb-8cd6-8c5d8330ad16`, decision request `c3cf1f8d-f695-473b-a122-058b5accfd11`). The zoom dialog D5 and D6 accepted by Cheese (`01a19293-4843-7853-8c95-9828c2a9c610`, decision request `73b93d4e-0816-499f-9656-cf3d104e0d13`). UX verdicts are in `LOCK.json` (x99-agy-gemini-ux).
Approved when the lock PR merges. The topic's last implementing PR deletes these reference files.

- `tree-minimap-states.html` (frame `HZbKCzDxOj`): the sidebar Hierarchy minimap, Direction A. Sidebar 320 and 260 px, light and dark, loading skeleton, root, leaf and 58 siblings. Leaf style D.
- `navigate-collapse.html` (frame `61GIbGrYNt`): navigating from the dialog collapses it into the target's minimap, 180 ms ease-out, instant under reduced motion.
- `dialog-zoomed-out.html` (frame `Kb3NjW8ywR`): canvas dialog at 40%. Each group is one block with a total ("21 siblings", "33 children") and a status bar. The current issue stays a full card.
- `dialog-zoomed-in.html` (frame `wJLp5mllsX`): canvas dialog at 130%. Groups split into single issue cards. Section labels carry the totals ("SIBLINGS · 21 (showing 8)").
- Each file is the frame as read from doop, unchanged. Its hash is `screens[].content_sha256` in `LOCK.json`.
- Implementation requirements from the UX review (not drawn): groups split at 70% zoom; pan and zoom need keyboard control (arrow keys, `+`, `-`) and focus placement on dialog open and on collapse; titles clamp to 2 lines in the sidebar at 260 px; the layout is computed from the hierarchy size, not fixed positions; the overview marks where unseen cards sit.
- Dropped and not locked: Directions B (Outline) and C (Strip); dialog frames D1 to D3 (old grouped leaf).
