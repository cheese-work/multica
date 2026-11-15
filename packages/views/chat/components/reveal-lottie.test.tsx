import { render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import animation from "../assets/reveal-happy.json";
import { RevealLottie } from "./reveal-lottie";

// jsdom has no canvas or WASM: the player is a marker so the test sees only
// which branch the component picked.
vi.mock("@lottiefiles/dotlottie-react", () => ({
  DotLottieReact: (props: { loop?: boolean }) => (
    <div data-testid="lottie-player" data-loop={String(!!props.loop)} />
  ),
}));

function setReducedMotion(reduce: boolean) {
  vi.spyOn(window, "matchMedia").mockImplementation(
    (query: string) =>
      ({
        matches: reduce && query === "(prefers-reduced-motion: reduce)",
        media: query,
        addEventListener: () => {},
        removeEventListener: () => {},
      }) as unknown as MediaQueryList,
  );
}

afterEach(() => vi.restoreAllMocks());

describe("RevealLottie", () => {
  it("reduced motion: static frame, no player", () => {
    setReducedMotion(true);
    render(<RevealLottie />);
    expect(screen.getByTestId("reveal-static-frame")).toBeInTheDocument();
    expect(screen.queryByTestId("lottie-player")).toBeNull();
  });

  it("motion allowed: plays once, no static frame", () => {
    setReducedMotion(false);
    render(<RevealLottie />);
    expect(screen.getByTestId("lottie-player")).toHaveAttribute("data-loop", "false");
    expect(screen.queryByTestId("reveal-static-frame")).toBeNull();
  });

  it("is decorative: hidden from assistive tech and never takes pointer events", () => {
    setReducedMotion(false);
    const { container } = render(<RevealLottie />);
    expect(container.firstElementChild).toHaveAttribute("aria-hidden", "true");
    expect(container.firstElementChild).toHaveClass("pointer-events-none");
  });

  it("asset runs under 1.5 s (plan section 3.3)", () => {
    expect((animation.op - animation.ip) / animation.fr).toBeLessThan(1.5);
  });
});
