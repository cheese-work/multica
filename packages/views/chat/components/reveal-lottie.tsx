import { DotLottieReact } from "@lottiefiles/dotlottie-react";
import { useReducedMotion } from "@multica/core/appearance";
import { cn } from "@multica/ui/lib/utils";
import animationData from "../assets/reveal-happy.json";

// Happy reveal (plan section 3.3): plays once, 1.2 s, decorative, never blocks.
// Reduced motion draws the animation's last frame as inline SVG and never
// loads the player or its WASM. The SVG mirrors the last frame of
// ../assets/reveal-happy.json (ring r=70, check path, same green).
export function RevealLottie({ className }: { className?: string }) {
  const wrap = cn("pointer-events-none", className);
  const reduced = useReducedMotion();
  if (reduced) {
    return (
      <div aria-hidden="true" className={wrap}>
        <svg
          data-testid="reveal-static-frame"
          viewBox="0 0 200 200"
          fill="none"
          stroke="#22c55e"
          strokeLinecap="round"
          strokeLinejoin="round"
        >
          <circle cx="100" cy="100" r="70" strokeWidth="10" />
          <path d="M70 102 L90 122 L132 78" strokeWidth="12" />
        </svg>
      </div>
    );
  }
  return (
    <div aria-hidden="true" className={wrap}>
      <DotLottieReact data={animationData} autoplay loop={false} />
    </div>
  );
}
