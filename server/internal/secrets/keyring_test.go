package secrets

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/secrets/seal"
)

func b64(fill byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32)) }

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadKeyring_Unset(t *testing.T) {
	ring, status, err := LoadKeyring(env(nil))
	if ring != nil || status != KeyUnset || err != nil {
		t.Fatalf("got ring=%v status=%v err=%v, want nil, KeyUnset, nil", ring, status, err)
	}
	_, status, _ = LoadKeyring(env(map[string]string{EnvKey: "  \n"}))
	if status != KeyUnset {
		t.Fatalf("whitespace-only key: status=%v, want KeyUnset", status)
	}
}

func TestLoadKeyring_Malformed(t *testing.T) {
	for _, v := range []string{"not-base64!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		ring, status, err := LoadKeyring(env(map[string]string{EnvKey: v}))
		if ring != nil || status != KeyMalformed || err == nil {
			t.Fatalf("%q: got ring=%v status=%v err=%v, want nil, KeyMalformed, error", v, ring, status, err)
		}
		if strings.Contains(err.Error(), v) {
			t.Fatalf("error leaks key value: %v", err)
		}
	}
}

func TestLoadKeyring_OK(t *testing.T) {
	ring, status, err := LoadKeyring(env(map[string]string{EnvKey: " " + b64(0x42) + "\n"}))
	if err != nil || status != KeyOK || ring == nil {
		t.Fatalf("got ring=%v status=%v err=%v", ring, status, err)
	}
	binding := seal.Binding{WorkspaceID: "w", CredentialID: "c", Purpose: "p"}
	sealed, err := ring.Seal([]byte("v"), binding)
	if err != nil {
		t.Fatal(err)
	}
	if want := seal.KeyID(bytes.Repeat([]byte{0x42}, 32)); sealed.KeyID != want {
		t.Fatalf("key id %q, want %q", sealed.KeyID, want)
	}
}

func TestLoadKeyring_Previous(t *testing.T) {
	ring, status, err := LoadKeyring(env(map[string]string{EnvKey: b64(0x42), EnvKeyPrevious: b64(0x24)}))
	if err != nil || status != KeyOK {
		t.Fatalf("status=%v err=%v", status, err)
	}
	// An envelope sealed under the previous key must still open and be rewrapped.
	prevMaterial := bytes.Repeat([]byte{0x24}, 32)
	old, err := seal.NewKeyring(seal.Key{ID: seal.KeyID(prevMaterial), Material: prevMaterial})
	if err != nil {
		t.Fatal(err)
	}
	binding := seal.Binding{WorkspaceID: "w", CredentialID: "c", Purpose: "p"}
	envOld, err := old.Seal([]byte("v"), binding)
	if err != nil {
		t.Fatal(err)
	}
	pt, repl, err := ring.OpenAndRotate(envOld, binding)
	if err != nil || string(pt) != "v" || repl == nil {
		t.Fatalf("rotate: pt=%q repl=%v err=%v", pt, repl, err)
	}
}

func TestLoadKeyring_PreviousEqualsCurrent(t *testing.T) {
	ring, status, err := LoadKeyring(env(map[string]string{EnvKey: b64(0x42), EnvKeyPrevious: b64(0x42)}))
	if err != nil || status != KeyOK || ring == nil {
		t.Fatalf("duplicate previous must be skipped: status=%v err=%v", status, err)
	}
}

func TestLoadKeyring_PreviousMalformed(t *testing.T) {
	ring, status, err := LoadKeyring(env(map[string]string{EnvKey: b64(0x42), EnvKeyPrevious: "nope!"}))
	if ring != nil || status != KeyMalformed || err == nil {
		t.Fatalf("got ring=%v status=%v err=%v", ring, status, err)
	}
}

func TestLoadKeyring_PreviousWithoutCurrentIsUnset(t *testing.T) {
	_, status, _ := LoadKeyring(env(map[string]string{EnvKeyPrevious: b64(0x24)}))
	if status != KeyUnset {
		t.Fatalf("status=%v, want KeyUnset", status)
	}
}

func TestLoadKeyring_NeverLogsKeyMaterial(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cur, prev := b64(0x42), b64(0x24)
	for _, m := range []map[string]string{
		{EnvKey: cur, EnvKeyPrevious: prev},
		{EnvKey: "bad-" + cur, EnvKeyPrevious: prev},
		{EnvKey: cur, EnvKeyPrevious: "bad-" + prev},
	} {
		_, status, err := LoadKeyring(env(m))
		if err != nil {
			logger.Error("load failed", "error", err)
		}
		LogKeyStatus(logger, status)
	}
	if buf.Len() == 0 {
		t.Fatal("expected log output")
	}
	for _, secret := range []string{cur, prev, "bad-" + cur, "bad-" + prev} {
		if strings.Contains(buf.String(), secret) {
			t.Fatalf("log output contains key material %q", secret)
		}
	}
}
