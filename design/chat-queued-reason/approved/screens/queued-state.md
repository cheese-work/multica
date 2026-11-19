# Queued state

Copied from the doop frame `Queued state · doc` (`TnHs6UIRJ_`, `updatedAt` 2026-11-19T02:04:00.108Z UTC). This doc frame is not in the waiver frame list; the reviewer read it.

## Purpose

Tell the person who sent a DM message why it is not running yet, when the provider is on hold, and that nothing is lost. Frames: queued with 1 message, queued with 2, recovered.

## UX reasoning

Today a send during a hold gets a 409 toast or silence. Now the send is accepted and a line under the message says what is waiting and why: _Queued: Claude is temporarily unavailable_, then what happens next (_Your message above runs automatically when the provider recovers_) and the next check time in ICT. The person need not retry or watch. The status sits directly under the held message, so it is clear which one waits. The first message keeps the status line; later messages are shown once, in the existing queue panel, labelled _Message 2 · runs after the message above_, so the order is explicit and no second explanation is repeated. When the provider recovers the line returns to the normal Starting up pill.

## UI reasoning

One new pill stage in the existing status slot. It uses the warning tint token at 16 %, the same one the offline banner uses for "unstable", and an icon plus words, so colour is never the only cue. The queue panel, message bubbles, composer and header are the existing parts, unchanged. No new colour, size, radius or layout.

## Ergonomics

The composer stays enabled: Send is always one tap. Send is 32 px on desktop and 44 px on a phone. The status line is read-only text with no target to miss. The queue row keeps its 24 px actions on desktop; on a phone they grow to 44 px (phone frame).

## States

Held (1 message), held (2 messages), checking now, still unavailable with the last check time, provider name unknown (falls back to "the model provider"), recovered and running. Browser offline keeps the existing offline banner, which wins over the hold line.

## Motion

Frame change is a 160 ms dissolve. The hold line appears in 160 ms ease-out with a 4 px rise. The checking dot pulses at 1.4 s. The caption swaps in 120 ms when a check fails. Each has a reduced-motion fallback: an instant swap or a static dot.

## Accessibility

The pill is `role=status` with `aria-live=polite`, so a screen reader hears it once when it appears and again when it changes to checking. The text carries the full meaning, so the icon is decorative. Times are written in ICT. Warning text on the tint is at least 4.5:1. Focus order is unchanged: transcript, queue rows, composer.
