package webhookguard

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	secret = []byte("whs_current")
	now    = time.Unix(1_800_000_000, 0)
	nonce  = "abcdefghijklmnop"
	body   = []byte(`{"issue":"CHE-1","result":"success"}`)
)

func ts(offset time.Duration) string { return strconv.FormatInt(now.Add(offset).Unix(), 10) }

func TestVerifyAcceptsValidSignature(t *testing.T) {
	sig := Sign(secret, ts(0), nonce, body)
	if !strings.HasPrefix(sig, "v1=") {
		t.Fatalf("signature %q lacks v1= prefix", sig)
	}
	if err := Verify([][]byte{secret}, now, ts(0), nonce, sig, body); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
}

func TestVerifyRejectsBadSignature(t *testing.T) {
	good := Sign(secret, ts(0), nonce, body)
	cases := map[string]struct {
		secrets [][]byte
		ts, n   string
		sig     string
		body    []byte
	}{
		"wrong secret":      {[][]byte{[]byte("other")}, ts(0), nonce, good, body},
		"tampered body":     {[][]byte{secret}, ts(0), nonce, good, []byte(`{"issue":"CHE-2","result":"success"}`)},
		"changed timestamp": {[][]byte{secret}, ts(1 * time.Second), nonce, good, body},
		"changed nonce":     {[][]byte{secret}, ts(0), "abcdefghijklmnoq", good, body},
		"no secrets":        {nil, ts(0), nonce, good, body},
		"wrong hex":         {[][]byte{secret}, ts(0), nonce, "v1=" + strings.Repeat("0", 64), body},
	}
	for name, c := range cases {
		err := Verify(c.secrets, now, c.ts, c.n, c.sig, c.body)
		if !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: got %v, want ErrUnauthorized", name, err)
		}
	}
}

func TestVerifyTimestampWindow(t *testing.T) {
	for name, off := range map[string]time.Duration{
		"too old": -MaxSkew - time.Second, "too new": MaxSkew + time.Second,
	} {
		sig := Sign(secret, ts(off), nonce, body)
		if err := Verify([][]byte{secret}, now, ts(off), nonce, sig, body); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: got %v, want ErrUnauthorized", name, err)
		}
	}
	for name, off := range map[string]time.Duration{"edge old": -MaxSkew, "edge new": MaxSkew} {
		sig := Sign(secret, ts(off), nonce, body)
		if err := Verify([][]byte{secret}, now, ts(off), nonce, sig, body); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestVerifyRotationAcceptsPreviousSecret(t *testing.T) {
	prev := []byte("whs_previous")
	sig := Sign(prev, ts(0), nonce, body)
	if err := Verify([][]byte{secret, prev}, now, ts(0), nonce, sig, body); err != nil {
		t.Fatalf("previous secret rejected: %v", err)
	}
	if err := Verify([][]byte{secret}, now, ts(0), nonce, sig, body); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("previous secret accepted after overlap: %v", err)
	}
}

func TestVerifyMalformedHeaders(t *testing.T) {
	sig := Sign(secret, ts(0), nonce, body)
	cases := map[string]struct{ ts, n, sig string }{
		"missing timestamp": {"", nonce, sig},
		"non-numeric ts":    {"yesterday", nonce, sig},
		"missing nonce":     {ts(0), "", sig},
		"short nonce":       {ts(0), "abc", sig},
		"long nonce":        {ts(0), strings.Repeat("a", 65), sig},
		"nonce bad charset": {ts(0), "abcdefghijklmn p", sig},
		"missing signature": {ts(0), nonce, ""},
		"no v1 prefix":      {ts(0), nonce, strings.TrimPrefix(sig, "v1=")},
		"non-hex signature": {ts(0), nonce, "v1=zz"},
	}
	for name, c := range cases {
		if err := Verify([][]byte{secret}, now, c.ts, c.n, c.sig, body); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: got %v, want ErrBadRequest", name, err)
		}
	}
}

func TestParsePayload(t *testing.T) {
	p, err := ParsePayload([]byte(`{"issue":"CHE-123","result":"failure","ref":"run-9_a.b/c:d"}`))
	if err != nil || p.Issue != "CHE-123" || p.Result != ResultFailure || p.Ref != "run-9_a.b/c:d" {
		t.Fatalf("got %+v, %v", p, err)
	}
	if _, err := ParsePayload([]byte(`{"issue":"CHE-1","result":"success"}`)); err != nil {
		t.Fatalf("ref must be optional: %v", err)
	}
	bad := map[string]string{
		"unknown field": `{"issue":"CHE-1","result":"success","extra":1}`,
		"bad result":    `{"issue":"CHE-1","result":"maybe"}`,
		"no issue":      `{"result":"success"}`,
		"bad issue":     `{"issue":"not an issue","result":"success"}`,
		"ref space":     `{"issue":"CHE-1","result":"success","ref":"a b"}`,
		"ref too long":  `{"issue":"CHE-1","result":"success","ref":"` + strings.Repeat("a", MaxRefLen+1) + `"}`,
		"not json":      `nope`,
		"trailing data": `{"issue":"CHE-1","result":"success"} {}`,
		"empty":         ``,
	}
	for name, b := range bad {
		if _, err := ParsePayload([]byte(b)); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: got %v, want ErrBadRequest", name, err)
		}
	}
	if _, err := ParsePayload([]byte(`{"issue":"CHE-1","result":"success","ref":"` + strings.Repeat("a", MaxRefLen) + `"}`)); err != nil {
		t.Errorf("ref at limit rejected: %v", err)
	}
}
