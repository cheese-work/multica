# CHE-2116 evidence

Real `ChatMessageList` in a throwaway Vite harness (not committed). 36 messages; an assistant message at row 14 mentions the viewer. Each case: scroll to the top with the wheel, screenshot, click the button, screenshot.

- `*-1-button.png`: button visible below the viewport mention.
- `*-2-after-jump.png`: the mention (`@ChEeSe` badge) is on screen and the button is gone.

Web = 1440x900. Mobile web = 390x844 viewport with a mobile user agent. Not covered: real touch scrolling (Playwright wheel and touch gestures did not scroll at device scale 2, so captures use scale 1), the issue-thread view, native mobile.
