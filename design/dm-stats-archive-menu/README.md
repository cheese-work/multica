## Request

Cheese, in the CHE-1755 request ([issue](mention://issue/01a13fcc-9e0d-7d27-aa33-aad915db21b7)):

> feat(multica): DM /archive command
> feat(multica): DM session statistic as an (i) icon in more menu and /statistic command
> feat(multica): DM /pin command

Addendum A2, Cheese, 2026-10-16: "allow user sent message from a archived DM with confirmation dialog to unarchive the DM first"; "archived DMs dont have pinned categories. They all unpinned"; "add /unarchive command". Taste decision T1 (D5): a second "Whole conversation" total next to "This DM only".

DES-1 asks one combined design pass for L2 (web and desktop) and L4 (mobile) before either layer is built.

- Issue: [CHE-1782](mention://issue/01a140a5-478f-7942-b804-7f35fc7ac10f)
- Pages in scope: web header more menu, floating chat menu, Statistics dialog (all states), slash suggestions, unarchive-on-send dialog, thread list and Archived view; mobile ⋯ menu, Statistics sheet, chat list with Archived section, unarchive alert
- Status: selected rev-2 direction C — approved when PR #284 merges
