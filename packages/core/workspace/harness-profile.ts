import { z } from "zod";

/**
 * Runtime profile (stored as `harness_profile`): how the daemon builds the
 * agent CLI's config home. `managed + sandbox` is reserved and never sent.
 * The key differs from the daemon "runtime profile" on purpose.
 */
export type HarnessProfile = "personal" | "managed";

export const HARNESS_PROFILES: readonly HarnessProfile[] = ["personal", "managed"];

const HarnessProfileSchema = z.enum(["personal", "managed"]).catch("personal");

export function readHarnessProfile(
  settings: Record<string, unknown> | undefined,
): HarnessProfile {
  return HarnessProfileSchema.parse(settings?.harness_profile);
}

/**
 * Effective profile, mirroring the server: agent override > workspace default
 * > personal. A null or malformed override inherits.
 */
export function resolveHarnessProfile(
  workspaceSettings: Record<string, unknown> | undefined,
  agentRuntimeConfig: Record<string, unknown> | undefined,
): HarnessProfile {
  const override = agentRuntimeConfig?.harness_profile;
  return override === "personal" || override === "managed"
    ? override
    : readHarnessProfile(workspaceSettings);
}
