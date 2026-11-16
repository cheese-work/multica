# Lottie player health check (CHE-5994, B0 of CHE-5480)

Checked 15 Nov 2026 ICT, with `npm view`, the npm downloads API and the GitHub REST API.

| Candidate | Latest | Last publish (UTC date) | Weekly downloads | License |
| --- | --- | --- | --- | --- |
| `@lottiefiles/dotlottie-react` (official LottieFiles player) | 0.22.2 | 12 Nov 2026 | 1,913,605 | MIT |
| `lottie-react` | 3.1.2 | 8 Sep 2026 | 4,489,905 | MIT |
| `lottie-web` (Airbnb) | 5.13.0 | 21 May 2025 | not checked | MIT |

`dotlottie-web` (repo behind the official player): not archived, 947 stars, 22 open issues, commits on 12 Nov 2026, the last 7 React wrapper releases span 28 Aug to 12 Nov 2026.

## Decision

Use `@lottiefiles/dotlottie-react@0.22.2` (plan section 3.8: prefer the official player). It is the only candidate released in the last 2 weeks and its one runtime dependency is `dotlottie-web`. `lottie-web` is stale (18 months).

## Known cost, for B3

- The player loads `dotlottie-player.wasm` (1.2 MB) from `cdn.jsdelivr.net`, with `unpkg.com` as fallback, unless `DotLottie.setWasmUrl(...)` points elsewhere. Self-hosted installs without internet egress would show no animation. B3 decides: keep the CDN default, or host the file.
- The component never loads the player under reduced motion and never blocks the reveal.

## Asset

`packages/views/chat/assets/reveal-happy.json`: original, hand-authored, 1.2 s, plays once. License file next to it. No paid or generated asset.
