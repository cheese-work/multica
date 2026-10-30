/**
 * Client-side fallback for redacting sensitive information in agent output.
 * The server performs primary redaction; this is a safety net for display.
 */

const patterns: { re: RegExp; replacement: string }[] = [
  // AWS access key IDs
  { re: /\bAKIA[0-9A-Z]{16}\b/g, replacement: "[REDACTED AWS KEY]" },
  // AWS secret access keys
  { re: /(?:aws_secret_access_key|secret_?access_?key)\s*[=:]\s*[A-Za-z0-9/+=]{40}/gi, replacement: "[REDACTED AWS SECRET]" },
  // PEM private keys
  { re: /-----BEGIN[A-Z\s]*PRIVATE KEY-----[\s\S]*?-----END[A-Z\s]*PRIVATE KEY-----/g, replacement: "[REDACTED PRIVATE KEY]" },
  // GitHub OAuth / classic tokens (ghp_/gho_/ghu_/ghs_/ghr_)
  { re: /\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9_]{36,255}\b/g, replacement: "[REDACTED GITHUB TOKEN]" },
  // GitHub fine-grained PATs (github_pat_…, recommended since 2022)
  { re: /\bgithub_pat_[A-Za-z0-9_]{20,255}\b/g, replacement: "[REDACTED GITHUB TOKEN]" },
  // Google API keys (AIza…, e.g. Gemini / Maps / Firebase)
  { re: /\bAIza[0-9A-Za-z_-]{35}([^0-9A-Za-z_-]|$)/g, replacement: "[REDACTED GOOGLE API KEY]$1" },
  // GitLab personal access tokens
  { re: /\bglpat-[A-Za-z0-9_-]{20,}\b/g, replacement: "[REDACTED GITLAB TOKEN]" },
  // OpenAI / Anthropic API keys
  { re: /\bsk-[A-Za-z0-9_-]{20,}\b/g, replacement: "[REDACTED API KEY]" },
  // Slack tokens
  { re: /\bxox[bporas]-[A-Za-z0-9-]{10,}\b/g, replacement: "[REDACTED SLACK TOKEN]" },
  // JWT tokens
  { re: /\bey[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b/g, replacement: "[REDACTED JWT]" },
  // Bearer tokens
  { re: /\bBearer\s+[A-Za-z0-9\-._~+/]+=*/gi, replacement: "Bearer [REDACTED]" },
  // Connection strings with embedded passwords
  { re: /(?:postgres|mysql|mongodb|redis|amqp)(?:ql)?:\/\/[^:\s]+:[^@\s]+@/gi, replacement: "[REDACTED CONNECTION STRING]@" },
];

const genericCredentialPattern = /(?:API_KEY|API_SECRET|SECRET_KEY|SECRET|ACCESS_TOKEN|AUTH_TOKEN|PRIVATE_KEY|DATABASE_URL|DB_PASSWORD|DB_URL|REDIS_URL|PASSWORD|TOKEN)\s*[=:]\s*/gi;
const credentialReferencePattern = /^(?:\$(?:[A-Za-z_][A-Za-z0-9_]*|\{[A-Za-z_][A-Za-z0-9_]*\})|<[A-Za-z_][A-Za-z0-9_-]*>|\*{3,})/;

function credentialReferenceLength(value: string): number {
  if (!value.startsWith("$(")) {
    return credentialReferencePattern.exec(value)?.[0].length ?? 0;
  }
  let depth = 0;
  let quote = "";
  for (let index = 1; index < value.length; index++) {
    const character = value[index];
    if (character === "\\" && quote !== "'") {
      index++;
      continue;
    }
    if (quote) {
      if (character === quote) quote = "";
      continue;
    }
    switch (character) {
      case "'":
      case '"':
        quote = character;
        break;
      case "(":
        depth++;
        break;
      case ")":
        depth--;
        if (depth === 0) return index + 1;
        break;
    }
  }
  return 0;
}

function isCredentialReference(input: string): boolean {
  let value = input;
  let quote = "";
  if (value.startsWith("'") || value.startsWith('"')) {
    quote = value[0]!;
    value = value.slice(1);
  }
  const end = credentialReferenceLength(value);
  if (!end) return false;
  let rest = value.slice(end);
  if (quote) {
    if (!rest.startsWith(quote)) return false;
    rest = rest.slice(1);
  }
  return rest.length === 0 || " \t\r\n\f;|&".includes(rest[0]!);
}

function redactGenericCredentials(text: string): string {
  const parts: string[] = [];
  let end = 0;
  for (const match of text.matchAll(genericCredentialPattern)) {
    if (match.index < end) continue;
    const value = text.slice(match.index + match[0].length);
    const literal = /^\S+/.exec(value)?.[0];
    if (!literal || isCredentialReference(value)) continue;
    parts.push(text.slice(end, match.index), "[REDACTED CREDENTIAL]");
    end = match.index + match[0].length + literal.length;
  }
  if (!end) return text;
  parts.push(text.slice(end));
  return parts.join("");
}

export function redactSecrets(text: string): string {
  let result = text;
  for (const { re, replacement } of patterns) {
    result = result.replace(re, replacement);
  }
  return redactGenericCredentials(result);
}
