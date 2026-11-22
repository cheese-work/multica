import { api, ApiError } from "../api";
import { signPasskeyChallenge } from "../auth/webauthn";
import type { GuardPolicyVersion } from "../api/passkey-schema";

export type GuardPolicyBody = Record<string, unknown>;

/** The policy changed since the page read it: the version now stored. */
export class GuardPolicyConflict extends Error {
  constructor(readonly current: GuardPolicyVersion) {
    super("guard policy changed");
    this.name = "GuardPolicyConflict";
  }
}

/** The person closed or timed out the passkey prompt: nothing was sent. */
export class GuardPasskeyCancelled extends Error {
  constructor() {
    super("passkey cancelled");
    this.name = "GuardPasskeyCancelled";
  }
}

function conflictFrom(error: unknown): GuardPolicyConflict | null {
  if (!(error instanceof ApiError) || error.status !== 409) return null;
  const body = error.body as { policy?: unknown; current_sha256?: unknown; code?: unknown } | undefined;
  if (body?.code !== "stale_base" || typeof body.current_sha256 !== "string") return null;
  const policy = body.policy && typeof body.policy === "object" && !Array.isArray(body.policy) ? (body.policy as GuardPolicyBody) : null;
  return new GuardPolicyConflict({ policy, sha256: body.current_sha256 });
}

/**
 * Challenge, sign with the passkey, write. The challenge binds the base digest and
 * the policy, so the server refuses a stale base before the signature is spent.
 * Throws `GuardPolicyConflict` on 409 and `GuardPasskeyCancelled` when the prompt is dismissed.
 */
export async function saveGuardPolicy(wsId: string, input: { policy: GuardPolicyBody; base_sha256: string }): Promise<GuardPolicyVersion> {
  try {
    const challenge = await api.challengeGuardPolicy(wsId, input);
    let assertion;
    try {
      assertion = await signPasskeyChallenge(challenge);
    } catch {
      throw new GuardPasskeyCancelled();
    }
    return await api.putGuardPolicy(wsId, { ...input, assertion });
  } catch (error) {
    throw conflictFrom(error) ?? error;
  }
}
