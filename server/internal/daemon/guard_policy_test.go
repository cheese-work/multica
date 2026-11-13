package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
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

func TestWriteGuardPolicyNoopWhenNoneStored(t *testing.T) {
	for name, settings := range map[string]string{
		"no settings": ``,
		"no key":      `{"other":1}`,
		"null key":    `{"guard_policy":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{}
			if err := writeGuardPolicy(env, t.TempDir(), []byte(settings)); err != nil {
				t.Fatal(err)
			}
			if len(env) != 0 {
				t.Fatalf("env set without a policy: %v", env)
			}
		})
	}
}

// A policy that is stored but cannot be delivered must not leave the guard on
// its permissive defaults: the env still points at a policy the guard cannot
// verify, so the guard fails closed (CHE-5204).
func TestWriteGuardPolicyFailureFailsClosed(t *testing.T) {
	cases := map[string]struct {
		settings string
		dir      func(*testing.T) string
	}{
		"invalid policy": {`{"guard_policy":{"version":9}}`, func(t *testing.T) string { return t.TempDir() }},
		"bad settings":   {`{`, func(t *testing.T) string { return t.TempDir() }},
		"unwritable dir": {guardSettings, func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing") }},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{}
			if err := writeGuardPolicy(env, c.dir(t), []byte(c.settings)); err == nil {
				t.Fatal("failure not reported")
			}
			if env[guard.PolicyEnv] == "" {
				t.Fatalf("guard left on its defaults: env %v", env)
			}
			pol, warn := guard.PolicyFromEnv(func(k string) string { return env[k] })
			if warn == "" {
				t.Fatal("guard accepted an undeliverable policy")
			}
			if pl := pol.Plugins[0]; pl.Name != "gh-pr" || pl.Action != guard.ActionDeny {
				t.Fatalf("gh pr not refused: %+v", pl)
			}
		})
	}
}
