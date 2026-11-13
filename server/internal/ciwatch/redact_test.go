package ciwatch

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

const (
	pemBody  = "MIIEowIBAAKCAQEAplantedSECRETkeybody0123456789"
	ghToken  = "ghp_" + "PLANTEDsecretTOKEN0123456789abcdefABCD"
	awsKey   = "AKIA" + "PLANTEDKEY123456"
	jwtToken = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJwbGFudGVkIn0.c2lnbmF0dXJlcGxhbnRlZA"
)

func redact(t *testing.T, in string, oneByte bool) string {
	t.Helper()
	var r io.Reader = strings.NewReader(in)
	if oneByte {
		r = iotest.OneByteReader(r)
	}
	lines, err := RedactTail(r, 200)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

func noLeak(t *testing.T, got string, planted ...string) {
	t.Helper()
	for _, p := range planted {
		if strings.Contains(got, p) {
			t.Errorf("leaked %q in:\n%s", p, got)
		}
	}
}

func TestRedactMultilineKeyEvenSplitAcrossChunks(t *testing.T) {
	in := "before\n2026-11-06T04:50:00Z -----BEGIN " + "RSA PRIVATE KEY-----\n" + pemBody + "\nline2" + pemBody + "\n-----END RSA PRIVATE KEY-----\nafter\n"
	for _, oneByte := range []bool{false, true} {
		got := redact(t, in, oneByte)
		noLeak(t, got, pemBody, "BEGIN RSA", "END RSA")
		if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
			t.Errorf("oneByte=%v lost surrounding lines: %q", oneByte, got)
		}
	}
}

func TestRedactUnterminatedBeginDropsToEnd(t *testing.T) {
	got := redact(t, "keep\n-----BEGIN "+"OPENSSH PRIVATE KEY-----\n"+pemBody+"\nmore\n", false)
	noLeak(t, got, pemBody, "more")
	if !strings.Contains(got, "keep") {
		t.Errorf("lost line before BEGIN: %q", got)
	}
}

func TestRedactEndWithoutBeginDropsFromLogStart(t *testing.T) {
	got := redact(t, "early secret "+pemBody+"\n"+pemBody+"\n-----END RSA PRIVATE KEY-----\nlater\n", false)
	noLeak(t, got, pemBody, "early secret")
	if !strings.Contains(got, "later") {
		t.Errorf("lost line after END: %q", got)
	}
}

func TestRedactSingleLineKeyAndOverlongLineMarker(t *testing.T) {
	one := "k=-----BEGIN " + "PRIVATE KEY-----\\n" + pemBody + "\\n-----END PRIVATE KEY-----\n"
	noLeak(t, redact(t, one, false), pemBody)
	// BEGIN sits past the 64 KiB line cap; the body on the next lines must still go.
	long := strings.Repeat("x", 200_000) + "-----BEGIN " + "PRIVATE KEY-----\n" + pemBody + "\n-----END PRIVATE KEY-----\nok\n"
	got := redact(t, long, true)
	noLeak(t, got, pemBody)
	if !strings.Contains(got, "ok") {
		t.Errorf("lost trailing line: %q", got)
	}
}

func TestRedactTokenShapes(t *testing.T) {
	in := strings.Join([]string{
		"token " + ghToken,
		"pat github_pat_11ABCDEFG0abcdefghijklmnopqrstuvwxyz_0123456789",
		"aws " + awsKey,
		"jwt " + jwtToken,
		"Authorization: Bearer abcDEF123456.tokenvalue",
		"clone https://user:s3cretPass@github.com/org/repo.git",
		"clone https://" + ghToken + "@github.com/org/repo.git",
		"masked ***",
	}, "\n")
	got := redact(t, in, false)
	noLeak(t, got, ghToken, "github_pat_11", awsKey, jwtToken, "abcDEF123456", "s3cretPass")
	if !strings.Contains(got, "masked ***") {
		t.Errorf("GitHub mask changed: %q", got)
	}
}

func TestRedactKeepsOnlyLastLines(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 1000; i++ {
		b.WriteString("line\n")
	}
	b.WriteString("last")
	lines, err := RedactTail(strings.NewReader(b.String()), 200)
	if err != nil || len(lines) != 200 || lines[len(lines)-1] != "last" {
		t.Fatalf("len=%d err=%v", len(lines), err)
	}
}

func TestLimitTailKeepsWholeLinesFromTheEnd(t *testing.T) {
	got := LimitTail([]string{"aaaa", "bbbb", "cccc"}, 10)
	if got != "bbbb\ncccc" {
		t.Errorf("got %q", got)
	}
	if got := LimitTail([]string{strings.Repeat("z", 50)}, 10); len(got) != 10 {
		t.Errorf("single long line not cut to 10 bytes: %d", len(got))
	}
}

// A line that closes one key and opens the next must keep the next key's body
// out of storage (CHE-4274 review P1).
func TestRedactEndThenBeginOnOneLineKeepsNextKeyClosed(t *testing.T) {
	second := "SECONDkeyBODYplanted0123456789"
	cases := map[string]string{
		"inside a key": "-----BEGIN " + "PRIVATE KEY-----\n" + pemBody + "\n-----END PRIVATE KEY----- -----BEGIN " + "PRIVATE KEY-----\n" + second + "\n",
		"stray end":    "x\n-----END PRIVATE KEY----- -----BEGIN " + "PRIVATE KEY-----\n" + second + "\n",
		"begin end begin": "-----BEGIN " + "PRIVATE KEY----- -----END PRIVATE KEY----- -----BEGIN " + "PRIVATE KEY-----\n" +
			second + "\n",
	}
	for name, in := range cases {
		for _, oneByte := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/oneByte=%v", name, oneByte), func(t *testing.T) {
				noLeak(t, redact(t, in, oneByte), pemBody, second)
			})
		}
	}
}

// A mismatched END label must not close an open key (CHE-4274 review P1).
func TestRedactMismatchedEndKeepsKeyOpenToEOF(t *testing.T) {
	cases := map[string]string{
		"public end":   "-----BEGIN " + "PRIVATE KEY-----\n-----END PUBLIC KEY-----\n" + pemBody + "\n",
		"rsa vs plain": "-----BEGIN " + "RSA PRIVATE KEY-----\n-----END PRIVATE KEY-----\n" + pemBody + "\n",
		"same line":    "-----BEGIN " + "PRIVATE KEY----- -----END PUBLIC KEY-----\n" + pemBody + "\n",
	}
	for name, in := range cases {
		for _, oneByte := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/oneByte=%v", name, oneByte), func(t *testing.T) {
				noLeak(t, redact(t, in, oneByte), pemBody)
			})
		}
	}
	// Matching labels still close, so later lines survive.
	got := redact(t, "-----BEGIN "+"PUBLIC KEY-----\n"+pemBody+"\n-----END PUBLIC KEY-----\nafter\n", false)
	if !strings.Contains(got, "after") {
		t.Errorf("matching END did not close: %q", got)
	}
}

// URL credentials whose "@" sits past the line cap must not be stored (CHE-4274 review P2).
func TestRedactOverlongLineDoesNotStoreCutCredential(t *testing.T) {
	in := "x https://user:hunter2planted" + strings.Repeat("p", 70_000) + "@host/x.git\nok\n"
	got := redact(t, in, false)
	noLeak(t, got, "hunter2planted")
	if !strings.Contains(got, "ok") {
		t.Errorf("lost trailing line: %q", got)
	}
}

// A nested BEGIN must not replace the open key's label (CHE-4274 review P1, round 3).
func TestRedactNestedBeginKeepsOuterKeyOpen(t *testing.T) {
	cases := map[string]string{
		"separate lines": "-----BEGIN " + "PRIVATE KEY-----\n-----BEGIN " + "PUBLIC KEY-----\n-----END PUBLIC KEY-----\n" + pemBody + "\n",
		"one line":       "-----BEGIN " + "PRIVATE KEY----- -----BEGIN " + "PUBLIC KEY----- -----END PUBLIC KEY-----\n" + pemBody + "\n",
	}
	for name, in := range cases {
		for _, oneByte := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/oneByte=%v", name, oneByte), func(t *testing.T) {
				noLeak(t, redact(t, in, oneByte), pemBody)
			})
		}
	}
	// The outer END cannot be told apart from the inner one, so the key stays open to EOF.
	got := redact(t, "-----BEGIN "+"PRIVATE KEY-----\n-----BEGIN "+"PUBLIC KEY-----\n"+pemBody+"\n-----END PRIVATE KEY-----\n"+pemBody+"\n", false)
	noLeak(t, got, pemBody)
}

// Two BEGINs and one END must not close the outer key (CHE-4274 review P1, round 4).
func TestRedactSameLabelNestedBeginFailsClosed(t *testing.T) {
	cases := map[string]string{
		"separate lines": "-----BEGIN " + "PRIVATE KEY-----\n-----BEGIN " + "PRIVATE KEY-----\n-----END PRIVATE KEY-----\n" + pemBody + "\n",
		"one line":       "-----BEGIN " + "PRIVATE KEY----- -----BEGIN " + "PRIVATE KEY----- -----END PRIVATE KEY-----\n" + pemBody + "\n",
	}
	for name, in := range cases {
		for _, oneByte := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/oneByte=%v", name, oneByte), func(t *testing.T) {
				noLeak(t, redact(t, in, oneByte), pemBody)
			})
		}
	}
}

// Generic and provider credentials follow the shared server/pkg/redact rules (CHE-4274 review P2).
func TestRedactAppliesSharedCredentialRules(t *testing.T) {
	got := redact(t, "DB_PASSWORD=hunter2planted\nAPI_KEY: abcplantedsecret99\nkept line\n", false)
	noLeak(t, got, "hunter2planted", "abcplantedsecret99")
	if !strings.Contains(got, "kept line") {
		t.Errorf("lost plain line: %q", got)
	}
}

// A provider-token-shaped username must not defeat URL-credential redaction:
// replacing the username first would leave the password behind.
func TestRedactURLCredentialsWithProviderShapedUsername(t *testing.T) {
	const pw = "plantedPASSWORD99"
	in := "git clone https://" + ghToken + ":" + pw + "@example.com/org/repo.git\n" +
		"fetch https://" + awsKey + ":" + pw + "@example.com/x\n"
	for _, oneByte := range []bool{false, true} {
		got := redact(t, in, oneByte)
		noLeak(t, got, pw, ghToken, awsKey)
		if !strings.Contains(got, "@example.com/org/repo.git") {
			t.Errorf("host/path must survive: %s", got)
		}
	}
}

// Every key/value form a CI log can carry a credential in, for each key class.
func TestRedactCredentialKeyValueForms(t *testing.T) {
	const secret = "plantedVALUE42xyz"
	forms := map[string]string{
		"json":                  `{"%[1]s":"%[2]s"}`,
		"json spaced":           `{"%[1]s": "%[2]s"}`,
		"single quoted":         `'%[1]s': '%[2]s'`,
		"bare colon":            `%[1]s: %[2]s`,
		"bare equals":           `%[1]s=%[2]s`,
		"inside longer json":    `{"id":7,"%[1]s":"%[2]s","ok":true,"name":"build"}`,
		"escaped quote value":   `{"%[1]s":"head\"%[2]s\"tail"}`,
		"json inside a string":  `msg="{\"%[1]s\":\"%[2]s\"}"`,
		"value with spaces":     `{"%[1]s":"%[2]s and more"}`,
		"quoted bare colon":     `%[1]s: "%[2]s and more"`,
		"lower case key in url": `?%[1]s=%[2]s&x=1`,
	}
	keys := []string{"DB_PASSWORD", "API_KEY", "token", "secret"}
	for name, form := range forms {
		for _, key := range keys {
			for _, oneByte := range []bool{false, true} {
				in := "prefix line\n" + fmt.Sprintf(form, key, secret) + "\nsuffix line\n"
				got := redact(t, in, oneByte)
				noLeak(t, got, secret, "plantedVALUE", "and more", "tail")
				if !strings.Contains(got, key) {
					t.Errorf("%s/%s: key name must stay visible: %s", name, key, got)
				}
			}
		}
	}
}

func TestRedactKeepsHarmlessJSON(t *testing.T) {
	in := `{"id":7,"name":"build","tokens_used":0,"ok":true}` + "\n"
	if got := redact(t, in, false); got != strings.TrimSuffix(in, "\n") {
		t.Errorf("harmless JSON changed: %s", got)
	}
}

// One rule: credential-like key (bare, single- or double-quoted), then : or =,
// then a value of any type. The value is masked whatever its shape.
func TestRedactCredentialValueTypes(t *testing.T) {
	cases := []struct{ in, planted string }{
		{`"password": hunter2planted`, "hunter2planted"},
		{`{"password":123456789}`, "123456789"},
		{`{"password":true}`, "true"},
		{`{"password":null}`, "null"},
		{`{"password":-1.5e3}`, "1.5e3"},
		{`'password': hunter2planted`, "hunter2planted"},
		{`password: "x planted y"`, "planted"},
		{`password = hunter2planted`, "hunter2planted"},
		{`password=hunter2planted&next=1`, "hunter2planted"},
		{`{"a":1,"API_KEY": 99887766,"b":2}`, "99887766"},
		{`secret: [planted1, planted2]`, "planted1"},
		{`token: Bearer plantedBEARERvalue1`, "plantedBEARERvalue1"},
		{`{"token":"a\"planted\"b"}`, "planted"},
	}
	for _, tc := range cases {
		for _, oneByte := range []bool{false, true} {
			got := redact(t, "before\n"+tc.in+"\nafter\n", oneByte)
			noLeak(t, got, tc.planted)
			if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
				t.Errorf("%q: neighbour lines lost: %s", tc.in, got)
			}
		}
	}
}

func TestRedactKeepsNonCredentialKeysWithCredentialSubstrings(t *testing.T) {
	in := `{"max_tokens":4096,"tokens_used":12,"password_policy":"strict"}`
	if got := redact(t, in+"\n", false); got != in {
		t.Errorf("changed: %s", got)
	}
}

// Values that nest or sit on a later line: masking tracks depth, quotes and
// lines, and an unterminated value masks to EOF.
func TestRedactCredentialValuesSpanNestingAndLines(t *testing.T) {
	cases := []struct {
		name, in string
		planted  []string
		keep     []string // lines that must survive
	}{
		{"nested object", `{"password":{"nested":{"x":1},"raw":"planted1"},"id":7}`, []string{"planted1"}, []string{`"id":7`}},
		{"brace inside string", `{"password":"a}b\"planted1"}`, []string{"planted1"}, nil},
		{"value on next line", "\"password\":\nplanted1\nafter", []string{"planted1"}, []string{"after"}},
		{"yaml indented next line", "password:\n    planted1 planted2\nafter", []string{"planted1", "planted2"}, []string{"after"}},
		{"blank line between", "password:\n\nplanted1\nafter", []string{"planted1"}, []string{"after"}},
		{"object body on later lines", "\"password\": {\n  \"user\": \"planted1\",\n  \"inner\": {\"k\": \"planted2\"}\n}\nafter", []string{"planted1", "planted2"}, []string{"after"}},
		{"list across lines", "secret: [\n  planted1,\n  planted2\n]\nafter", []string{"planted1", "planted2"}, []string{"after"}},
		{"unterminated object masks to EOF", "password: {\nplanted1\nplanted2\n", []string{"planted1", "planted2"}, nil},
		{"unterminated list masks to EOF", "token = [\nplanted1\n", []string{"planted1"}, nil},
		{"unterminated string stays on its line", "password: \"planted1 planted2\nafter", []string{"planted1", "planted2"}, []string{"after"}},
		{"value then more json", `{"password":{"a":1},"id":7}` + "\nafter", nil, []string{`"id":7`, "after"}},
	}
	for _, tc := range cases {
		for _, oneByte := range []bool{false, true} {
			got := redact(t, "before\n"+tc.in+"\n", oneByte)
			noLeak(t, got, tc.planted...)
			for _, k := range append([]string{"before"}, tc.keep...) {
				if !strings.Contains(got, k) {
					t.Errorf("%s: %q lost: %s", tc.name, k, got)
				}
			}
		}
	}
}
