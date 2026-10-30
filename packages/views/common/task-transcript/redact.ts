/**
 * Client-side fallback for redacting sensitive information in agent output.
 * The server performs primary redaction; this is a safety net for display.
 */

import providerRules from "../../../../server/pkg/redact/provider-rules.json";

// Provider-token rules are shared with the server (server/pkg/redact embeds the
// same file); the parity fixture in redact.test.ts keeps both on one list.
const patterns = (
  providerRules as { pattern: string; flags: string; replacement: string }[]
).map(({ pattern, flags, replacement }) => ({ re: new RegExp(pattern, `${flags}g`), replacement }));

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
