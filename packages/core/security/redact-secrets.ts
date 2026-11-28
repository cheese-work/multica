/**
 * Client-side fallback for redacting sensitive information in agent output.
 * The server performs primary redaction; this is a safety net for display.
 */

import providerRules from "../../../server/pkg/redact/provider-rules.json";

// Provider-token rules are shared with the server (server/pkg/redact embeds the
// same file); the parity fixture in redact.test.ts keeps both on one list.
// The file uses Go replacement syntax (`${1}`); String.prototype.replace only
// expands `$1`, so group references are translated here.
const patterns = (providerRules as { pattern: string; flags: string; replacement: string }[]).map(
  ({ pattern, flags, replacement }) => ({
    re: new RegExp(pattern, `${flags}g`),
    replacement: replacement.replace(/\$\{(\d+)\}/g, "$$$1"),
  }),
);

const genericCredentialPattern = /(?:API_KEY|API_SECRET|SECRET_KEY|SECRET|ACCESS_TOKEN|AUTH_TOKEN|PRIVATE_KEY|DATABASE_URL|DB_PASSWORD|DB_URL|REDIS_URL|PASSWORD|TOKEN)\s*[=:]\s*/gi;
// Prose-safe subset of the keyword rule. Agent prose says "the password field" and
// "| Token: input, output |", so a bare keyword never triggers on its own. It needs an
// assignment shape: compound names (API_KEY, DB_PASSWORD) with "=" or ":", a bare
// keyword with "=" (spaces allowed), or a bare keyword with ":" followed by a
// secret-looking token (6+ chars with a digit).
const proseCredentialPattern = /(?:API_KEY|API_SECRET|SECRET_KEY|ACCESS_TOKEN|AUTH_TOKEN|PRIVATE_KEY|DATABASE_URL|DB_PASSWORD|DB_URL|REDIS_URL)[ \t]*[=:][ \t]*|(?:SECRET|PASSWORD|TOKEN)[ \t]*=[ \t]*|(?:SECRET|PASSWORD|TOKEN)[ \t]*:[ \t]*(?=\S*\d)(?=\S{6})/gi;
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

function redactGenericCredentials(text: string, pattern = genericCredentialPattern): string {
  const parts: string[] = [];
  let end = 0;
  for (const match of text.matchAll(pattern)) {
    if (match.index < end) continue;
    const value = text.slice(match.index + match[0].length);
    const literal = /^\S+/.exec(value)?.[0];
    if (!literal || isCredentialReference(value)) continue;
    // Keep "KEY=" so a reader can see which credential was masked.
    parts.push(text.slice(end, match.index + match[0].length), "[REDACTED]");
    end = match.index + match[0].length + literal.length;
  }
  if (!end) return text;
  parts.push(text.slice(end));
  return parts.join("");
}

function redactProviderTokens(text: string): string {
  let result = text;
  for (const { re, replacement } of patterns) {
    result = result.replace(re, replacement);
  }
  return result;
}

/** Provider-token rules plus the prose-safe credential rule: for agent prose, where the full keyword rule would mangle ordinary sentences. */
export function redactProse(text: string): string {
  return redactGenericCredentials(redactProviderTokens(text), proseCredentialPattern);
}

/** Provider-token rules plus the full keyword rule: for machine output (tool input, output, errors). */
export function redactSecrets(text: string): string {
  return redactGenericCredentials(redactProviderTokens(text));
}

/** Pick the rule set by transcript item type; only `text` and `thinking` are agent prose. */
export function redactForType(type: string, text: string): string {
  return type === "text" || type === "thinking" ? redactProse(text) : redactSecrets(text);
}
