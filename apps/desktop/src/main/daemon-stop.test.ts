// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { CLI_STOP_WAIT_MS } from "./daemon-profile";
import { execDaemonStop, restartAfterStop } from "./daemon-stop";

// Fake `execFile` that behaves like Node's: the CLI exits after `drainMs`, but
// the child is killed with an error first if `options.timeout` elapses.
function fakeExec(drainMs: number) {
  return (
    _file: string,
    _args: string[],
    options: { timeout: number },
    callback: (err: Error | null) => void,
  ) => {
    if (drainMs > options.timeout) {
      setTimeout(() => callback(new Error("Command failed: killed")), options.timeout);
    } else {
      setTimeout(() => callback(null), drainMs);
    }
  };
}

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe("restart during a long daemon drain", () => {
  it("starts the replacement when the stop takes longer than 15s", async () => {
    const start = vi.fn(async () => ({ success: true }));
    const restart = restartAfterStop(
      () => execDaemonStop("multica", ["daemon", "stop"], fakeExec(CLI_STOP_WAIT_MS)),
      start,
    );
    await vi.advanceTimersByTimeAsync(CLI_STOP_WAIT_MS);
    expect(await restart).toEqual({ success: true });
    expect(start).toHaveBeenCalledTimes(1);
  });

  it("starts no replacement when the stop is killed by the exec timeout", async () => {
    const start = vi.fn(async () => ({ success: true }));
    const restart = restartAfterStop(
      () => execDaemonStop("multica", ["daemon", "stop"], fakeExec(10 * CLI_STOP_WAIT_MS)),
      start,
    );
    await vi.advanceTimersByTimeAsync(10 * CLI_STOP_WAIT_MS);
    expect((await restart).success).toBe(false);
    expect(start).not.toHaveBeenCalled();
  });
});
