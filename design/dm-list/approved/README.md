Locked from doop page `mRAnnjvGod:che-5511` (file https://congvc-c00.taila6fa8a.ts.net:34443/c/mRAnnjvGod), frames `HWWDBRkKMd` (`2026-11-13T15:14:48.149Z`), `efRMEvOLwO` (`2026-11-13T15:15:08.636Z`) and `vudJBbhks4` (`2026-11-13T15:14:47.254Z`); times 13 Nov 2026 22:14, 22:15 and 22:14 ICT. Issue [CHE-5511](mention://issue/01a1d46a-95a3-7a47-b2ff-887d63b61906).
Direction A, v3. Cheese chose direction A (DM, 13 Nov 2026 17:18 ICT) and "Fix the 5 notes first" (decision card `83c4b2f4-5ecc-4bfe-aa82-ac1d41278fbf`, 13 Nov 2026 21:48 ICT), recorded in CHE-5511 comment `01a1d561-edca-7776-8345-ad9e615cdc8f`. UX PASS on these exact versions (`01a1d583-7640-7010-96a4-464cbdb8ee9c`, x99-codex-terra, no P0/P1; every criterion scored 7 to 9).
Approved when the lock PR merges. The topic's last implementing PR deletes these reference files.

## What this lock holds

- `dm-list-main.html`: desktop list with filter chips (`Tất cả`, `Hộp chính` default, `Quyết định`, `Chờ bạn`, `Chưa xem`, `Ghim / không ghim`, `Lưu trữ`), sticky day headers, bold unseen with a blue dot, `⋯` on hover or focus, right-click menu and `U P E R X` shortcuts; mobile two-way swipe, always-visible 44×44 `⋯`, swipe-archive with the Hoàn tác toast, long-press select mode with the icon-only split bar; group select with tri-state header checkbox and split buttons; one ⌘K palette with chips and the result rules.
- `states-desktop.html`: loading, empty, error with Thử lại, offline, for the list and ⌘K, with a 2px focus ring on Thử lại.
- `states-mobile.html`: the same four list states at 390 px, plus four phone ⌘K states (searching, no results, error, offline).
- All text is at least 12 px; phone controls are at least 44 px, except the checkboxes below.

## Implementation must add and test

- Checkbox hit area (Terra's P2 note, the biggest problem for A): the selected-row and group checkboxes are drawn at 22 px. Give each a hit area of at least 44×44 and verify it in the build.
- Gestures, focus order and screen-reader output were not tested (static HTML): swipe, long-press, roving focus in the chip row, and names on icon-only buttons (Đã xem, Ghim, Lưu trữ) need real tests.
- The 2px `:focus-visible` ring is drawn on one control only. Every interactive element needs it in the product.
- Reduced motion stops the skeleton and spinner animation.
- Map the mockup's colour values to semantic tokens in `packages/ui/styles/tokens.css`; the mockup copies values and the code must not.
- All copy needs every supported translation. The mockup is in Vietnamese.

## Not covered by this lock

- v1 and v2 on source canvas `CyYJVz2cBk`; they stay as history.
- Light theme is not mocked.
- Live data, long titles, and reflow between 390 px and 1440 px are not verified.
