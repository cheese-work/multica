package wecom

// progress_secrets_multica_test.go — the application's own credentials and the
// AWS secret key must not reach a bubble (CHE-3456 security review, ISSUE-1).
// All values are synthetic.

import (
	"strings"
	"testing"
)

// The three formats jwt.go issues: prefix + 40 lowercase hex. Shape alone never
// flags them (one case, under minSecretRunLen), and mat_/mul_/mdt_ are not in
// the vendor prefix list, so each needs its own rule.
var multicaTokens = []string{
	"mat_0123456789abcdef0123456789abcdef01234567",
	"mul_0123456789abcdef0123456789abcdef01234567",
	"mdt_abcdefabcdefabcdefabcdefabcdefabcdefabcd",
}

const awsSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" // 40 chars

func TestMulticaAndAWSCredentialsNeverReachTheBubble(t *testing.T) {
	var cases []struct{ name, in, gone string }
	for _, tok := range multicaTokens {
		cases = append(cases,
			struct{ name, in, gone string }{"reasoning " + tok[:4], "Inspecting " + tok + " next.", tok},
			struct{ name, in, gone string }{"tool arg " + tok[:4], `curl -H "X: ` + tok + `" https://example.com`, tok},
		)
	}
	cases = append(cases,
		struct{ name, in, gone string }{"aws positional", "aws configure set aws_secret_access_key " + awsSecret, awsSecret},
		struct{ name, in, gone string }{"aws session token", "aws configure set aws_session_token " + strings.Repeat("a", 40), strings.Repeat("a", 40)},
	)
	// Quoted values, quoted names and a shell line continuation (security review
	// of e63748c29: the first pattern stopped at the opening quote).
	for _, in := range []string{
		`aws configure set aws_secret_access_key "` + awsSecret + `"`,
		`aws configure set aws_secret_access_key '` + awsSecret + `'`,
		`aws configure set "aws_secret_access_key" "` + awsSecret + `"`,
		"aws configure set aws_secret_access_key \\\n  " + awsSecret,
		`aws configure set aws_session_token "` + awsSecret + `"`,
	} {
		cases = append(cases,
			struct{ name, in, gone string }{"quoted reasoning " + in[:48], "Running: " + in + " now.", awsSecret},
			struct{ name, in, gone string }{"quoted tool arg " + in[:48], in, awsSecret},
		)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, got := range []string{redactSecrets(tc.in), safeFragment(tc.in), tidyThinking(tc.in)} {
				if strings.Contains(got, tc.gone) {
					t.Fatalf("credential survives: %q", got)
				}
			}
		})
	}
}

func TestMulticaTokenSplitAcrossIncrementsIsCaught(t *testing.T) {
	for _, tok := range multicaTokens {
		for cut := 5; cut < len(tok); cut += 7 {
			chunks := []string{"I'll use " + tok[:cut], tok[cut:] + " to call the API."}
			for i, frame := range thinkingFrames(t, chunks...) {
				// No frame may show the prefix plus 8 characters of the secret.
				if strings.Contains(frame, tok[:12]) {
					t.Errorf("%s cut %d frame %d carries the token: %q", tok[:4], cut, i, frame)
				}
			}
		}
	}
}

func TestQuotedAWSSecretSplitAcrossIncrementsIsCaught(t *testing.T) {
	for _, q := range []string{`"`, `'`, ``} {
		full := "aws configure set aws_secret_access_key " + q + awsSecret + q
		for cut := 30; cut < len(full); cut += 5 {
			chunks := []string{"Running " + full[:cut], full[cut:] + " now."}
			for i, frame := range thinkingFrames(t, chunks...) {
				// No frame may show 8 or more characters of the secret.
				for j := 0; j+8 <= len(awsSecret); j++ {
					if strings.Contains(frame, awsSecret[j:j+8]) {
						t.Fatalf("quote %q cut %d frame %d carries the secret: %q", q, cut, i, frame)
					}
				}
			}
		}
	}
}

func TestOrdinaryIdentifiersSurviveTheMulticaRule(t *testing.T) {
	for _, in := range []string{
		"checkout 0123456789abcdef0123456789abcdef01234567",
		"lookup 7f3c2a9e-1b4d-4e8a-9c2f-3d5e6f7a8b9c",
		"call mul_add and mat_helper in utils.go",
	} {
		if got := redactSecrets(in); got != in {
			t.Errorf("ordinary text redacted: %q -> %q", in, got)
		}
	}
}
