package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/guard"
)

const guardSettings = `{"other":1,"guard_policy":{"version":1,"verify":false,
 "plugins":[{"name":"x","match":{"tool":"gh"},"action":"deny","message":"ws"}],
 "layers":[{"scope":"org","org":"acme","plugins":[{"name":"x","match":{"tool":"gh"},"action":"pass"}]}]}}`

func TestWriteGuardPolicyPublishesDigestCheckedFile(t *testing.T) {
	tmp := t.TempDir()
	env := map[string]string{}
	if err := writeGuardPolicy(env, tmp, []byte(guardSettings)); err != nil {
		t.Fatal(err)
	}
	file := env[guard.PolicyEnv]
	if filepath.Dir(file) != tmp {
		t.Fatalf("policy file %q is not in the task temp dir %q", file, tmp)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(raw); env[guard.PolicySHAEnv] != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest env %q does not match the file", env[guard.PolicySHAEnv])
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Fatalf("policy file mode %v, want 0600", fi.Mode().Perm())
	}
	pol, warn := guard.PolicyFromEnv(func(k string) string { return env[k] })
	if warn != "" {
		t.Fatalf("guard refused the daemon's policy: %s", warn)
	}
	if len(pol.Plugins) != 1 || pol.Plugins[0].Action != guard.ActionDeny || pol.Plugins[0].Message != "ws" {
		t.Fatalf("workspace level not resolved: %+v", pol.Plugins)
	}
}

func TestWriteGuardPolicyNoopAndInvalid(t *testing.T) {
	for name, settings := range map[string]string{
		"no settings":  ``,
		"no key":       `{"other":1}`,
		"null key":     `{"guard_policy":null}`,
		"bad settings": `{`,
	} {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{}
			if err := writeGuardPolicy(env, t.TempDir(), []byte(settings)); err != nil && name != "bad settings" {
				t.Fatal(err)
			}
			if len(env) != 0 {
				t.Fatalf("env set without a policy: %v", env)
			}
		})
	}
	env := map[string]string{}
	err := writeGuardPolicy(env, t.TempDir(), []byte(`{"guard_policy":{"version":9}}`))
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("invalid policy not reported: %v", err)
	}
	if len(env) != 0 {
		t.Fatalf("env set for an invalid policy: %v", env)
	}
}
