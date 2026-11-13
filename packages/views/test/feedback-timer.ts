import { vi } from "vitest";

/**
 * Records the 2s "Copied" feedback timers set from now on, so a test can assert
 * the component clears them on unmount instead of leaving them to fire after
 * the test environment is torn down.
 */
export function trackFeedbackTimers() {
  const ids: unknown[] = [];
  const realSetTimeout = window.setTimeout;
  const setTimeoutSpy = vi.spyOn(window, "setTimeout").mockImplementation(((
    ...args: Parameters<typeof window.setTimeout>
  ) => {
    const id = realSetTimeout(...args);
    if (args[1] === 2000) ids.push(id);
    return id;
  }) as typeof window.setTimeout);
  const clearTimeoutSpy = vi.spyOn(window, "clearTimeout");
  return {
    ids,
    cleared: (id: unknown) => clearTimeoutSpy.mock.calls.some(([cleared]) => cleared === id),
    restore: () => {
      setTimeoutSpy.mockRestore();
      clearTimeoutSpy.mockRestore();
    },
  };
}
