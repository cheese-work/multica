export interface LevelMotion { durationMs: number; easing: string; slide: boolean; transition: string }

/** Level-change keyframes locked in the design: 240 ms ease-out; reduced motion cross-fades over 0 ms with no slide. */
export const LEVEL_MS = 240;

export function levelMotion(reducedMotion: boolean): LevelMotion {
  if (reducedMotion) return { durationMs: 0, easing: "linear", slide: false, transition: "none" };
  return { durationMs: LEVEL_MS, easing: "ease-out", slide: true, transition: `transform ${LEVEL_MS}ms ease-out, opacity ${LEVEL_MS}ms ease-out` };
}
