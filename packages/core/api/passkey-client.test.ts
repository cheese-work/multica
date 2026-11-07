import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./client";

afterEach(() => vi.unstubAllGlobals());

const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
const envelope = { challenge_id: "ch", credential: { id: "a" } };
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

  it("sends the gate assertion with enrollment finish", async () => {
    const finish = client({ enrolled: true });
    await finish.api.finishPasskeyEnrollment({ ceremony_id: "c", code: "1", attestation: {}, assertion: envelope });
    expect(JSON.parse(finish.call().init.body).assertion).toEqual(envelope);
  });

  it("lists the caller's passkeys", async () => {
    const list = client({ passkeys: [{ id: "p1", created_at: "t", last_used_at: null, transports: [], state: "suspect" }] });
    expect(await list.api.listPasskeys()).toEqual([{ id: "p1", created_at: "t", last_used_at: null, transports: [], state: "suspect" }]);
    expect(list.call().url).toBe("https://api.example.test/api/passkeys");
    await expect(client({ passkeys: "x" }).api.listPasskeys()).rejects.toThrow(/unexpected/);
  });

  it("revokes a passkey with a mailed code", async () => {
    const start = client({ ceremony_id: "c", expires_at: "t" });
    expect(await start.api.startPasskeyRevocationByCode("p/1")).toEqual({ ceremony_id: "c", expires_at: "t" });
    expect(start.call()).toMatchObject({ url: "https://api.example.test/api/passkeys/p%2F1/revoke/code", init: { method: "POST" } });

    const confirm = client({ revoked: true });
    await confirm.api.confirmPasskeyRevocationByCode("p/1", { ceremony_id: "c", code: "123456" });
    expect(confirm.call().url).toBe("https://api.example.test/api/passkeys/p%2F1/revoke/confirm");
    expect(JSON.parse(confirm.call().init.body)).toEqual({ ceremony_id: "c", code: "123456" });
    await expect(client({ revoked: false }).api.confirmPasskeyRevocationByCode("p", { ceremony_id: "c", code: "1" })).rejects.toThrow(/unexpected/);
  });

  it("opens a revoke challenge and revokes with the assertion", async () => {
    const challenge = client({ challenge_id: "c", options, expires_at: "t" });
    await challenge.api.challengePasskeyRevocation("p/1");
    expect(challenge.call().url).toBe("https://api.example.test/api/passkeys/p%2F1/revoke/challenge");

    const revoke = client({ revoked: true });
    await revoke.api.revokePasskey("p1", envelope);
    expect(revoke.call().url).toBe("https://api.example.test/api/passkeys/p1/revoke");
    expect(JSON.parse(revoke.call().init.body)).toEqual({ assertion: envelope });
  });

  it("challenges, approves and reads the outcome of a decision action", async () => {
    const challenge = client({ challenge_id: "c", options, expires_at: "t" });
    await challenge.api.challengeDecisionAction("a1");
    expect(challenge.call().url).toBe("https://api.example.test/api/decision-actions/a1/challenge");

    const approve = client({ id: "a1", state: "executed" });
    await approve.api.approveDecisionAction("a1", { payload_sha256: "h", assertion: envelope });
    expect(approve.call().url).toBe("https://api.example.test/api/decision-actions/a1/approve");
    expect(JSON.parse(approve.call().init.body)).toEqual({ payload_sha256: "h", assertion: envelope });

    const outcome = client({ id: "a1", state: "pending" });
    expect((await outcome.api.getDecisionActionOutcome("a1")).state).toBe("pending");
    expect(outcome.call().url).toBe("https://api.example.test/api/decision-actions/a1/outcome");
  });

  it("opens a card-answer challenge and returns the revisions it binds", async () => {
    const challenge = client({ challenge_id: "c", options, expires_at: "t", answer_revision: 3, policy_revision: 2 });
    expect((await challenge.api.challengeDecisionAnswer("r/1", { option: "A", text: "why" })).answer_revision).toBe(3);
    expect(challenge.call().url).toBe("https://api.example.test/api/decision-requests/r%2F1/answer/challenge");
    expect(JSON.parse(challenge.call().init.body)).toEqual({ option: "A", text: "why" });

    const answer = client({});
    await answer.api.answerDecisionRequest("r1", { option: "A", answer_revision: 3, policy_revision: 2, assertion: envelope });
    expect(JSON.parse(answer.call().init.body)).toEqual({ option: "A", answer_revision: 3, policy_revision: 2, assertion: envelope });
    await expect(client({ challenge_id: "c", options, expires_at: "t" }).api.challengeDecisionAnswer("r1", {})).rejects.toThrow(/unexpected/);
  });

  it("throws instead of reporting success on a malformed response", async () => {
    await expect(client({ revoked: "yes" }).api.revokePasskey("p1", envelope)).rejects.toThrow(/unexpected/);
    await expect(client({ revoked: false }).api.revokePasskey("p1", envelope)).rejects.toThrow(/unexpected/);
    await expect(client({}).api.approveDecisionAction("a1", { payload_sha256: "h", assertion: envelope })).rejects.toThrow(/unexpected/);
  });
});
