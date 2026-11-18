import { z } from "zod";
import { parseWithFallback } from "./schema";

// Server options are `{ publicKey: {...} }` (go-webauthn); the ceremony helper
// reads the inner fields, so only the wrapper is required here.
const OptionsSchema = z.object({ publicKey: z.record(z.string(), z.unknown()) });

export const PasskeyChallengeSchema = z.object({ challenge_id: z.string(), options: OptionsSchema, expires_at: z.string() });
// `assertion` is present when the workspace gate blocks and the user already has a passkey.
// The revisions the challenge binds; the answer must carry the same pair.
export const PasskeyAnswerChallengeSchema = PasskeyChallengeSchema.extend({
  answer_revision: z.number().int(), policy_revision: z.number().int(),
});
export const PasskeyEnrollStartSchema = z.object({
  ceremony_id: z.string(), tag: z.string().optional(), options: OptionsSchema, expires_at: z.string(), assertion: PasskeyChallengeSchema.optional(),
});
export const PasskeyListSchema = z.object({
  passkeys: z.array(z.object({
    id: z.string(), created_at: z.string(), last_used_at: z.string().nullable(), transports: z.array(z.string()),
    state: z.enum(["active", "suspect"]),
  })),
});
export const PasskeyEnrolledSchema = z.object({ enrolled: z.literal(true) });
export const PasskeyRevokeCodeStartSchema = z.object({ ceremony_id: z.string(), tag: z.string().optional(), expires_at: z.string() });
export const PasskeyRevokedSchema = z.object({ revoked: z.literal(true) });
/** The non-secret card view of one action; `payload_sha256` is what an approve must repeat. */
export const DecisionActionViewSchema = z.object({
  id: z.string(),
  action_type: z.string(),
  params: z.record(z.string(), z.unknown()),
  payload_sha256: z.string(),
  state: z.string(),
  /** The chat that holds the action's card; older servers omit it. */
  chat_session_id: z.string().optional(),
});
/**
 * What a decided run-budget action did (CHE-6943 §3). `outcome` is an open set of tokens; each number
 * drops alone, and a receipt without its two required fields drops whole, so a bad one never hides the action.
 */
const optionalNumber = z.number().optional().catch(undefined);
export const BudgetReceiptSchema = z.object({
  outcome: z.string(),
  applied_at: z.string(),
  runs_used: optionalNumber,
  runs_limit: optionalNumber,
  runs_limit_after: optionalNumber,
  new_window: z.boolean().optional().catch(undefined),
}).optional().catch(undefined);
export type BudgetReceipt = NonNullable<z.infer<typeof BudgetReceiptSchema>>;
export const PasskeyDecisionOutcomeSchema = z.object({
  id: z.string(),
  state: z.string(),
  result: z.unknown().optional(),
  receipt: BudgetReceiptSchema,
  error_code: z.string().optional(),
  error_message: z.string().optional(),
});
/** The batch card's read after approve: one outcome per item. */
export const PasskeyBatchOutcomeSchema = z.object({ batch_id: z.string(), items: z.array(PasskeyDecisionOutcomeSchema) });
export type PasskeyBatchOutcome = z.infer<typeof PasskeyBatchOutcomeSchema>;

/** The stored guard policy (null when the workspace stores none) and its digest, the base an edit names. */
export const GuardPolicyVersionSchema = z.object({ policy: z.record(z.string(), z.unknown()).nullable(), sha256: z.string() });
export type GuardPolicyVersion = z.infer<typeof GuardPolicyVersionSchema>;

/** What the server verifies for revoke and approve: the challenge it answers plus the WebAuthn credential. */
export type PasskeyAssertionEnvelope = { challenge_id: string; credential: unknown };
export type PasskeyEnrollStart = z.infer<typeof PasskeyEnrollStartSchema>;
export type PasskeySummary = z.infer<typeof PasskeyListSchema>["passkeys"][number];
export type PasskeyChallenge = z.infer<typeof PasskeyChallengeSchema>;
export type DecisionActionView = z.infer<typeof DecisionActionViewSchema>;
export type PasskeyAnswerChallenge = z.infer<typeof PasskeyAnswerChallengeSchema>;
export type PasskeyDecisionOutcome = z.infer<typeof PasskeyDecisionOutcomeSchema>;

/**
 * Passkey responses gate a security decision, so a drifted shape must fail
 * loudly instead of degrading to an empty fallback the UI could read as success.
 * Uses `parseWithFallback` for the shared warning log, then throws on the null.
 */
export function parsePasskeyResponse<T>(raw: unknown, schema: z.ZodType<T>, endpoint: string): T {
  const parsed = parseWithFallback<T | null>(raw, schema, null, { endpoint });
  if (parsed === null) throw new Error(`unexpected response from ${endpoint}`);
  return parsed;
}
