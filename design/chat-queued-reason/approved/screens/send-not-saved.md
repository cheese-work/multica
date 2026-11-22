# Send not saved

Copied from the doop frame `Send not saved · doc` (`E0Bl-j8dm7`, `updatedAt` 2026-11-19T02:03:42.404Z UTC). This doc frame is not in the waiver frame list; the reviewer read it.

## Purpose

When a send did not reach the server, keep the draft, say what happened in one sentence and give one action. Frames: save failed with unknown delivery, refused with a specific reason.

## UX reasoning

Today a toast says "Failed to send message" and vanishes, and a second toast says "Couldn't confirm the send" with no action. The person cannot tell if the message went. The banner stays until the person acts. It says **Not confirmed** (delivery unknown) or **Not sent** (refused), why, that the draft is still in the box, and for the unknown case that the message may or may not be saved and that Retry is safe and will not send twice. The copy never says "message id": behind the scenes the client reuses the same id, as `pending-send.ts` already does, and the person does not need to know that. A refusal names the cause and a fix action: _x99-claude-gstack has no runtime bound_ with _Bind runtime_.

## UI reasoning

The banner sits in the offline-banner slot above the composer, with the destructive tint token at 10 % and the existing alert icon. The composer border turns to the destructive token so the draft and the error are tied. The two toasts are removed.

## Ergonomics

Retry is 24 px on desktop and 44 px on a phone. It sits at the end of the banner, next to the thumb on a phone, and directly above Send. Retry and Send do the same thing; Retry exists because the banner explains why.

## States

Delivery unknown (timeout, network error, 408, 5xx) with Retry; refused (4xx) with a specific reason and, when one exists, a fix action; retrying (Retry disabled, spinner, text Retrying); recovered (banner removed, message shows as sent).

## Motion

Banner reveal is 160 ms height and opacity. The send spinner and the Retrying spinner turn at 800 ms linear. Reduced motion: no transition, static text Retrying.

## Accessibility

The banner is `role=alert`, so it is announced when it appears. Retry has a visible text label and a 2 px focus ring with 2 px offset. Focus does not move when the banner appears; the draft is still the active element. The reason text is plain language, with no code or status number.
