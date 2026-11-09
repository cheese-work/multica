import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

// CHE-3701: a sealed report is unreadable without the private key and round-trips with it.
const dir = fileURLToPath(new URL(".", import.meta.url));
const run = (cmd, args, env) => execFileSync(cmd, args, { env: { ...process.env, ...env }, encoding: "utf8" });

test("seal.sh encrypts to the recipient, removes the plaintext; unseal.sh restores it", (t) => {
  assert.ok(existsSync(join(dir, "seal.sh")) && existsSync(join(dir, "unseal.sh")), "seal.sh and unseal.sh exist");
  const tmp = mkdtempSync(join(tmpdir(), "seal-"));
  const home = join(tmpdir(), `seal-home-${process.pid}`); // outside the sealed dir: the keyring has a trustdb.gpg too
  run("mkdir", ["-m", "700", home]);
  run("gpg", ["--batch", "--passphrase", "", "--quick-gen-key", "t <t@invalid>", "default", "default", "never"], { GNUPGHOME: home });
  const pub = run("gpg", ["--armor", "--export"], { GNUPGHOME: home });
  const fpr = run("gpg", ["--with-colons", "--list-keys"], { GNUPGHOME: home }).match(/^fpr:+([0-9A-F]{40}):/m)[1];
  writeFileSync(join(tmp, "recipient.asc"), pub);
  const report = join(tmp, "semgrep.json");
  t.after(() => run("rm", ["-rf", home, tmp], {}));
  writeFileSync(report, '{"results":[{"check_id":"secret-rule"}]}');

  run("bash", [join(dir, "seal.sh"), report], { SECURITY_WEEKLY_RECIPIENT: join(tmp, "recipient.asc"), SECURITY_WEEKLY_FPR: fpr });
  assert.ok(!existsSync(report), "plaintext removed");
  assert.doesNotMatch(readFileSync(`${report}.gpg`).toString("latin1"), /secret-rule/);

  const out = join(tmp, "out");
  run("bash", [join(dir, "unseal.sh"), "--local", tmp, out], { SECURITY_WEEKLY_GNUPGHOME: home });
  assert.equal(readFileSync(join(out, "semgrep.json"), "utf8"), '{"results":[{"check_id":"secret-rule"}]}');
});

// Luna's FAIL on cc18ed4c0: recipient.asc is PR-controlled, so a swapped key must not be trusted. The expected fingerprint
// is pinned outside the key file; seal.sh refuses any other key and leaves the plaintext in place.
test("seal.sh fails closed when the key is not the pinned fingerprint, or no pin is set", (t) => {
  const tmp = mkdtempSync(join(tmpdir(), "seal-pin-"));
  const home = join(tmpdir(), `seal-pin-home-${process.pid}`);
  run("mkdir", ["-m", "700", home]);
  run("gpg", ["--batch", "--passphrase", "", "--quick-gen-key", "attacker <a@invalid>", "default", "default", "never"], { GNUPGHOME: home });
  writeFileSync(join(tmp, "recipient.asc"), run("gpg", ["--armor", "--export"], { GNUPGHOME: home }));
  t.after(() => run("rm", ["-rf", home, tmp], {}));
  const report = join(tmp, "semgrep.json");
  writeFileSync(report, "{}");
  const seal = (env) => run("bash", [join(dir, "seal.sh"), report], { SECURITY_WEEKLY_RECIPIENT: join(tmp, "recipient.asc"), ...env });
  assert.throws(() => seal({ SECURITY_WEEKLY_FPR: "1A9377889F3E812D256D3612A32DB5293024988D" }), /does not match the pinned/);
  assert.throws(() => seal({ SECURITY_WEEKLY_FPR: "" }), /SECURITY_WEEKLY_FPR/);
  assert.ok(existsSync(report) && !existsSync(`${report}.gpg`), "plaintext kept, nothing written");
});
