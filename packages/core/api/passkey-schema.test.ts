// @vitest-environment node

import { describe, expect, it } from "vitest";
import {
  PasskeyChallengeSchema, PasskeyDecisionOutcomeSchema, PasskeyEnrollStartSchema,
  PasskeyEnrolledSchema, PasskeyListSchema, PasskeyRevokedSchema,
} from "./passkey-schema";

const options = { publicKey: { challenge: "AQ" } };
const cases = [
  ["enroll start", PasskeyEnrollStartSchema, { ceremony_id: "c", options, expires_at: "2026-11-01T00:00:00Z" },
    [null, {}, { ceremony_id: "c", options: {}, expires_at: "t" }, { ceremony_id: 1, options, expires_at: "t" }]],
  ["challenge", PasskeyChallengeSchema, { challenge_id: "c", options, expires_at: "t" },
    [null, {}, { challenge_id: "c", options: "x", expires_at: "t" }, { challenge_id: "c", options, expires_at: 5 }]],
  ["passkey list", PasskeyListSchema, { passkeys: [{ id: "p", created_at: "t", last_used_at: null, transports: ["usb"], state: "active" }] },
    [null, {}, { passkeys: "x" }, { passkeys: [{ id: "p", created_at: "t" }] }, { passkeys: [{ id: 1, created_at: "t", last_used_at: null, transports: [], state: "active" }] }, { passkeys: [{ id: "p", created_at: "t", last_used_at: null, transports: [], state: "gone" }] }]],
  ["enrolled", PasskeyEnrolledSchema, { enrolled: true }, [null, {}, { enrolled: "yes" }, { enrolled: false }]],
  ["revoked", PasskeyRevokedSchema, { revoked: true }, [null, {}, { revoked: "yes" }, { revoked: false }]],
  ["decision outcome", PasskeyDecisionOutcomeSchema, { id: "a", state: "executed" },
    [null, {}, { id: "a" }, { id: "a", state: 3 }, { id: "a", state: "x", error_code: 5 }]],
] as const;

describe.each(cases)("%s schema", (_name, schema, good, bad) => {
  it("accepts the server shape", () => expect(schema.safeParse(good).success).toBe(true));
  it("rejects malformed responses", () => {
    for (const value of bad) expect(schema.safeParse(value).success).toBe(false);
  });
});

it("keeps outcome result and error fields and ignores unknown ones", () => {
  const parsed = PasskeyDecisionOutcomeSchema.parse({ id: "a", state: "failed", result: { k: 1 }, error_code: "e", error_message: "m", extra: 1 });
  expect(parsed).toMatchObject({ result: { k: 1 }, error_code: "e", error_message: "m" });
});

it("keeps the gate assertion on enroll start", () => {
  const assertion = { challenge_id: "c", options, expires_at: "t" };
  expect(PasskeyEnrollStartSchema.parse({ ceremony_id: "c", options, expires_at: "t", assertion }).assertion).toEqual(assertion);
});
