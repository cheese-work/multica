package ciwatch

import (
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

const (
	pemBody  = "MIIEowIBAAKCAQEAplantedSECRETkeybody0123456789"
	ghToken  = "ghp_PLANTEDsecretTOKEN0123456789abcdefABCD"
	awsKey   = "AKIAPLANTEDKEY123456"
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
	in := "before\n2026-11-06T04:50:00Z -----BEGIN RSA PRIVATE KEY-----\n" + pemBody + "\nline2" + pemBody + "\n-----END RSA PRIVATE KEY-----\nafter\n"
	for _, oneByte := range []bool{false, true} {
		got := redact(t, in, oneByte)
		noLeak(t, got, pemBody, "BEGIN RSA", "END RSA")
		if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
			t.Errorf("oneByte=%v lost surrounding lines: %q", oneByte, got)
		}
	}
}

func TestRedactUnterminatedBeginDropsToEnd(t *testing.T) {
	got := redact(t, "keep\n-----BEGIN OPENSSH PRIVATE KEY-----\n"+pemBody+"\nmore\n", false)
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
	one := "k=-----BEGIN PRIVATE KEY-----\\n" + pemBody + "\\n-----END PRIVATE KEY-----\n"
	noLeak(t, redact(t, one, false), pemBody)
	// BEGIN sits past the 64 KiB line cap; the body on the next lines must still go.
	long := strings.Repeat("x", 200_000) + "-----BEGIN PRIVATE KEY-----\n" + pemBody + "\n-----END PRIVATE KEY-----\nok\n"
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
