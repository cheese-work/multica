## Ask thread (S-b) · doc — direction B, frame `YOhz1szjhV` `updatedAt` `2026-11-16T14:47:53.147Z`

Copied unchanged from doc frame `PfJIE4F9zc` (`updatedAt` `2026-11-18T15:15:48.584Z`). Frame S-b, shared by both directions, desktop 1440 and phone 390. "Ask first" on the card opens a small ask thread.

### Purpose
Let you ask the agent a question before you pick, without losing the open card, and bring you back to the card when the agent has answered.

### UX reasoning
The ask thread names where it came from ("From thread"), leaves a one-line stub under the card in the original thread, and ends with an "Answered. Back to the card to pick." line. The original card stays open the whole time and the Submit pick button stays usable, with the hint "You asked a question. Pick when ready."

### UI reasoning
The ask thread is an ordinary thread with a Back button and a "From thread" label, so it needs no new pattern. The loop-back line is a tinted strip with one filled button, the same style as the link row in B-b.

### Ergonomics
On phone, the stub row is 56 px and Back is 44 px. The loop-back button is at the end of the thread, within thumb reach after reading the answer.

### States
Stub in the original thread, ask thread opened, answered with the loop-back line. The ask thread follows the view rules: shown in Focus (it has you in it), one line in Follow, but never folded while its card waits.

### Motion
Opening the ask thread and going back use the 150 ms dissolve. With reduced motion, the swap is instant.

### Accessibility
Back is a button with a text label. The stub is a button with `aria-expanded` and the question in its name. The loop-back line is `role=status`, so the answer is announced. Focus returns to the stub after Back.
