import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./client";

afterEach(() => vi.unstubAllGlobals());

const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
const options = { publicKey: { challenge: "AQ" } };

function client(body: unknown) {
  const fetchMock = vi.fn().mockResolvedValue(json(body));
  vi.stubGlobal("fetch", fetchMock);
  return { api: new ApiClient("https://api.example.test"), call: () => ({ url: String(fetchMock.mock.calls[0]?.[0]), init: fetchMock.mock.calls[0]?.[1] }) };
}

describe("ApiClient passkeys", () => {
  it("starts and finishes enrollment", async () => {
    const start = client({ ceremony_id: "c", options, expires_at: "t" });
    expect((await start.api.startPasskeyEnrollment()).ceremony_id).toBe("c");
    expect(start.call()).toMatchObject({ url: "https://api.example.test/api/passkeys/enroll/start", init: { method: "POST" } });

    const finish = client({ enrolled: true });
    const attestation = { id: "x" };
    await finish.api.finishPasskeyEnrollment({ ceremony_id: "c", code: "123456", attestation });
    expect(finish.call().url).toBe("https://api.example.test/api/passkeys/enroll/finish");
    expect(JSON.parse(finish.call().init.body)).toEqual({ ceremony_id: "c", code: "123456", attestation });
  });

  it("opens a revoke challenge and revokes with the assertion", async () => {
    const challenge = client({ challenge_id: "c", options, expires_at: "t" });
    await challenge.api.challengePasskeyRevocation("p/1");
    expect(challenge.call().url).toBe("https://api.example.test/api/passkeys/p%2F1/revoke/challenge");

    const revoke = client({ revoked: true });
    await revoke.api.revokePasskey("p1", { id: "a" });
    expect(revoke.call().url).toBe("https://api.example.test/api/passkeys/p1/revoke");
    expect(JSON.parse(revoke.call().init.body)).toEqual({ assertion: { id: "a" } });
  });

  it("challenges, approves and reads the outcome of a decision action", async () => {
    const challenge = client({ challenge_id: "c", options, expires_at: "t" });
    await challenge.api.challengeDecisionAction("a1");
    expect(challenge.call().url).toBe("https://api.example.test/api/decision-actions/a1/challenge");

    const approve = client({ id: "a1", state: "executed" });
    await approve.api.approveDecisionAction("a1", { payload_sha256: "h", assertion: { id: "a" } });
    expect(approve.call().url).toBe("https://api.example.test/api/decision-actions/a1/approve");
    expect(JSON.parse(approve.call().init.body)).toEqual({ payload_sha256: "h", assertion: { id: "a" } });

    const outcome = client({ id: "a1", state: "pending" });
    expect((await outcome.api.getDecisionActionOutcome("a1")).state).toBe("pending");
    expect(outcome.call().url).toBe("https://api.example.test/api/decision-actions/a1/outcome");
  });

  it("throws instead of reporting success on a malformed response", async () => {
    await expect(client({ revoked: "yes" }).api.revokePasskey("p1", {})).rejects.toThrow(/unexpected/);
    await expect(client({}).api.approveDecisionAction("a1", { payload_sha256: "h", assertion: {} })).rejects.toThrow(/unexpected/);
  });
});
