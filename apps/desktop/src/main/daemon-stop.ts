import { execFile } from "child_process";

import { DAEMON_FORCE_STOP_EXEC_TIMEOUT_MS, DAEMON_STOP_EXEC_TIMEOUT_MS } from "./daemon-profile";

export type DaemonOpResult = { success: boolean; error?: string };

type ExecFile = (
  file: string,
  args: string[],
  options: { timeout: number },
  callback: (err: Error | null) => void,
) => unknown;

/** Run `multica daemon stop`; the exec timeout is what ends a long drain. */
export function execDaemonStop(
  bin: string,
  args: string[],
  exec: ExecFile = execFile as ExecFile,
): Promise<DaemonOpResult> {
  const timeout = args.includes("--force") ? DAEMON_FORCE_STOP_EXEC_TIMEOUT_MS : DAEMON_STOP_EXEC_TIMEOUT_MS;
  return new Promise((resolve) => {
    exec(bin, args, { timeout }, (err) => {
      resolve(err ? { success: false, error: err.message } : { success: true });
    });
  });
}

/** A failed stop ends the restart: no replacement daemon is started. */
export async function restartAfterStop(
  stop: () => Promise<DaemonOpResult>,
  start: () => Promise<DaemonOpResult>,
): Promise<DaemonOpResult> {
  const stopResult = await stop();
  if (!stopResult.success) return stopResult;
  return start();
}
