// Package redact provides functions for detecting and masking secrets
// in agent output before it reaches the database or WebSocket broadcast.
package redact

import (
	_ "embed"
	"encoding/json"
	"regexp"
	"strings"
)

// providerRulesJSON is the provider-token rule list shared with the
// TypeScript display fallback (packages/views/common/task-transcript/redact.ts
// imports the same file). Keep patterns in the RE2 / ECMAScript common subset.
// flags is a subset of "is": Go prefixes it as an inline (?flags) group and
// TypeScript passes it to RegExp.
//
//go:embed provider-rules.json
var providerRulesJSON []byte

// secretPattern pairs a compiled regex with its replacement text.
type secretPattern struct {
	re          *regexp.Regexp
	replacement string
}

// Patterns are checked in order; first match wins per position.
var patterns = loadProviderPatterns()

func loadProviderPatterns() []secretPattern {
	var rules []struct {
		Pattern     string `json:"pattern"`
		Flags       string `json:"flags"`
		Replacement string `json:"replacement"`
	}
	if err := json.Unmarshal(providerRulesJSON, &rules); err != nil {
		panic("redact: invalid provider-rules.json: " + err.Error())
	}
	out := make([]secretPattern, len(rules))
	for i, rule := range rules {
		expr := rule.Pattern
		if rule.Flags != "" {
			expr = "(?" + rule.Flags + ")" + expr
		}
		out[i] = secretPattern{regexp.MustCompile(expr), rule.Replacement}
	}
	return out
}

var genericCredentialPattern = regexp.MustCompile(`(?i)(?:API_KEY|API_SECRET|SECRET_KEY|SECRET|ACCESS_TOKEN|AUTH_TOKEN|PRIVATE_KEY|DATABASE_URL|DB_PASSWORD|DB_URL|REDIS_URL|PASSWORD|TOKEN)\s*[=:]\s*`)
var credentialValuePattern = regexp.MustCompile(`^\S+`)
var credentialReferencePattern = regexp.MustCompile(`^(?:\$(?:[A-Za-z_][A-Za-z0-9_]*|\{[A-Za-z_][A-Za-z0-9_]*\})|<[A-Za-z_][A-Za-z0-9_-]*>|\*{3,})`)

func credentialReferenceLength(value string) int {
	if !strings.HasPrefix(value, "$(") {
		return len(credentialReferencePattern.FindString(value))
	}
	depth, quote := 0, byte(0)
	for index := 1; index < len(value); index++ {
		character := value[index]
		if character == '\\' && quote != '\'' {
			index++
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			}
			continue
		}
		switch character {
		case '\'', '"':
			quote = character
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return index + 1
			}
		}
	}
	return 0
}

func isCredentialReference(value string) bool {
	quote := byte(0)
	if len(value) > 0 && (value[0] == '\'' || value[0] == '"') {
		quote, value = value[0], value[1:]
	}
	end := credentialReferenceLength(value)
	if end == 0 {
		return false
	}
	rest := value[end:]
	if quote != 0 {
		if len(rest) == 0 || rest[0] != quote {
			return false
		}
		rest = rest[1:]
	}
	return len(rest) == 0 || strings.ContainsRune(" \t\r\n\f;|&", rune(rest[0]))
}

func redactGenericCredentials(text string) string {
	var result strings.Builder
	end := 0
	for _, match := range genericCredentialPattern.FindAllStringIndex(text, -1) {
		if match[0] < end {
			continue
		}
		value := text[match[1]:]
		literal := credentialValuePattern.FindString(value)
		if literal == "" || isCredentialReference(value) {
			continue
		}
		// Keep "KEY=" so a reader can see which credential was masked.
		result.WriteString(text[end:match[1]])
		result.WriteString("[REDACTED]")
		end = match[1] + len(literal)
	}
	if end == 0 {
		return text
	}
	result.WriteString(text[end:])
	return result.String()
}

// maxRedactDepth bounds the walk in redactValue. Tool inputs are decoded from
// daemon-supplied JSON, so nesting depth is attacker-influenced; without a
// bound a pathologically nested payload would recurse until the stack blows and
// take the process down. Real tool inputs nest a handful of levels at most, so
// this only ever trips on abuse.
const maxRedactDepth = 32

// depthLimitPlaceholder replaces anything below maxRedactDepth. Returning the
// raw value there would hand back an unscrubbed string, which is exactly what
// this package exists to prevent, so the fail-safe direction is to drop it.
const depthLimitPlaceholder = "[REDACTED DEPTH LIMIT]"

// InputMap returns a copy of m with every string value passed through Text,
// including strings nested inside maps and slices.
//
// The nested walk is load-bearing, not defensive tidying: providers record
// structured tool inputs, and Codex records a file edit as
// changes[]{path, diff, content}. A top-level-only pass leaves a credential
// inside a patch body — or the full contents of a deleted .env — untouched on
// its way to the database and the WebSocket broadcast.
func InputMap(m map[string]any) map[string]any {
	return redactMap(m, 0)
}

func redactMap(m map[string]any, depth int) map[string]any {
	if m == nil {
		return nil
	}
	if depth >= maxRedactDepth {
		return map[string]any{"_": depthLimitPlaceholder}
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = redactValue(v, depth+1)
	}
	return out
}

// redactValue scrubs a single decoded JSON value, recursing through the
// composite shapes json.Unmarshal produces plus []string, which providers use
// for argv-style inputs.
//
// Composites are copied rather than scrubbed in place: the caller still holds
// the original map and keeps using it after redaction (the daemon handler logs
// and re-reads it), so mutating through the shared reference would be a
// surprise at a distance.
func redactValue(v any, depth int) any {
	if depth >= maxRedactDepth {
		return depthLimitPlaceholder
	}
	switch t := v.(type) {
	case string:
		return Text(t)
	case map[string]any:
		return redactMap(t, depth)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = redactValue(e, depth+1)
		}
		return out
	case []string:
		out := make([]string, len(t))
		for i, e := range t {
			out[i] = Text(e)
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(t))
		for k, e := range t {
			out[k] = Text(e)
		}
		return out
	default:
		return v
	}
}

// Text scans the input string for known secret patterns and replaces
// matches with safe placeholders.
//
// It deliberately does NOT mask the local home directory. That masking used to
// live here, but it was never a boundary: anyone who can read a transcript
// already sees repository paths, file contents, commands and diffs, so hiding
// one path segment protected nothing while making paths unusable for copy-paste
// and debugging. It was also incoherent about whose home directory it hid —
// Content/Output are redacted in the server's ingest handler, so a hosted
// deployment matched them against the *server's* home, never the machine
// running the agent. Transcript visibility is an authorization concern and is
// handled at that layer, not by string replacement here.
func Text(s string) string {
	return redactGenericCredentials(Prose(s))
}

// Prose applies only the provider-token rules. Use it for text an agent wrote
// for people to read (replies, summaries, comments): the generic keyword rule
// in Text reads label words such as "Token:" in a table cell as a credential
// and deletes the text after them (CHE-2587). Use Text for machine output.
func Prose(s string) string {
	for _, p := range patterns {
		s = p.re.ReplaceAllString(s, p.replacement)
	}
	return s
}
