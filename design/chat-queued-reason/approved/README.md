Locked from doop page [CHE-7316 · chat-queued-reason](https://congvc-c00.taila6fa8a.ts.net:34443/c/mRAnnjvGod?frame=pKtksZVCVg) (file https://congvc-c00.taila6fa8a.ts.net:34443/c/mRAnnjvGod), page `mRAnnjvGod:che-7316`. Issue [CHE-7316](mention://issue/01a1ef27-0761-7c4d-b008-64bd7bb425c2).

Direction A, selected by Cheese through the recorded pick on issue comment `01a1f189-1459-73c8-8b5c-9755a24facc1` (thread `01a1f09c-8227-704d-a9b7-8f750c09560a`). Version: v1.3, the eight frames in `LOCK.json` at the listed `updatedAt`.

## This lock is a waiver, not a PASS

The last UX verdict on v1.3 is a HOLD (CHE-7437 comment `01a1f1a8-cd40-7af2-bfc1-6c5421ea3f79`), not a PASS. The platform stopped a fourth review round. Cheese chose "Waive and lock" on decision card `d1490c53` (19 Nov 2026 ICT). The waiver covers these exact frame versions only; any later frame edit needs its own re-check or waiver.

Waived gaps:

- The doop completeness checks are not built: CHE-6853, CHE-6854, CHE-6628.
- There is no G3 attestation (`check_design_completeness`).
- The prototype click-through was not run end to end by a reviewer.
- The sloplint result is unadjudicated: 328 FAIL, 662 REFLEX, 42 WARN, unfiltered.

Not in the waiver frame list, but read by the reviewer: the docs `Queued state · doc` (`TnHs6UIRJ_`) and `Send not saved · doc` (`E0Bl-j8dm7`), and the notes frame `kZFtoDNvAf`. The docs are copied to `screens/`; the notes frame is not exported.

Lock PR publishes this record. The topic's last implementing PR deletes the `*.html` reference files; `screens/` stays.
