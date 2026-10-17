## Request

> Mention in issues, i notice by chance too, i comb through wall of text of messages in issue thread.

— Cheese, 11 Oct 2026, office hours with x99-claude-opus (Demand Evidence in `design-dm-decision-requests.md`, attached to the first comment of CHE-1539). Layer 1 (backend) is merged; this topic designs layer 2 (UI) and layer 3 ("Waiting on you").

- Issue: [CHE-1840](mention://issue/01a1486d-a8d1-755f-b8c6-5bb20f33676d), child of [CHE-1539](mention://issue/01a12b39-4c52-7adc-8abc-082979d3e4c5)
- Sources: `design-dm-decision-requests.md` revision 14 and the CHE-1611 amendment `design-dm-lane-pick-by-mentions.md`; decisions option B (archived lanes), Q7 (explicit tagging only), Q8 A (agent-picked class on unprompted records), Q9 (veto details)
- Pages in scope:
  - `chat/ping-card` — the ask in the DM: issue key, the named person, the raise-class control (raise only, from Reversible to a 10-min hold class), the lane the agent picked or the new chat it requested
  - `chat/reference-note` — after a recorded decision: quote, Open issue, Disclaim, the 10-minute hold for the five escalation classes, the agent veto note (issue key, vetoing agent, reason)
  - `issue/decision-quote` — the attested quote on the issue thread: live, disclaimed (struck), vetoed, cancelled; the permalink to the original message, including archived chats
  - `inbox/waiting-on-you` — a tab of open requests for one person, one line each: ask, issue key, age; loading, empty, error
- Out of scope: lane summaries (removed, CHE-1610), continuation and `/compact` (CHE-1538), the Jev ranker, any backend change
- Status: rev-1 posted
