package plugincontract_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/plugincontract"
)

// The steel-browser plugin is the identity the Steel gateway authenticates
// with: its install token (mpi_) is the gateway credential. The plan (CHE-4695
// W1 revision 3, F0) fixes its shape, so this test pins that shape. It is not an
// example: it ships under plugins/, outside the examples that must stay
// copy-pasteable.
func steelBrowserDir() string {
	return filepath.Join("..", "..", "..", "plugins", "steel-browser")
}

func loadSteelBrowserBundle(t *testing.T) plugincontract.Bundle {
	t.Helper()
	root := steelBrowserDir()
	bundle, err := plugincontract.ParseBundleFromDir(func(entry string) ([]byte, bool, error) {
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(entry)))
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		return content, true, nil
	})
	if err != nil {
		t.Fatalf("steel-browser package does not parse as a plugin bundle: %v", err)
	}
	return bundle
}

func TestSteelBrowserManifestParsesAndInstallsOnThisHost(t *testing.T) {
	bundle := loadSteelBrowserBundle(t)
	if err := bundle.Manifest.CheckCapabilities(plugincontract.HostCapabilities()); err != nil {
		t.Fatalf("steel-browser declares something this host cannot run: %v", err)
	}
}

func TestSteelBrowserManifestIdentity(t *testing.T) {
	m := loadSteelBrowserBundle(t).Manifest
	if m.Key != "work.cheese.steel-browser" {
		t.Fatalf("key = %q, want work.cheese.steel-browser", m.Key)
	}
	if m.Version != "0.1.0" {
		t.Fatalf("version = %q, want 0.1.0", m.Version)
	}
}

// Pull only: the gateway calls Multica's internal listener. Multica never calls
// out, so the manifest declares no hook (a hook needs a public HTTPS endpoint,
// and tailnet hosts are refused), no net: scope and no config secret.
func TestSteelBrowserManifestIsPullOnly(t *testing.T) {
	m := loadSteelBrowserBundle(t).Manifest
	if len(m.Contributes.Hooks) != 0 {
		t.Fatalf("hooks = %d, want 0: Multica must not call out to the gateway", len(m.Contributes.Hooks))
	}
	if len(m.Contributes.Surfaces) != 0 {
		t.Fatalf("surfaces = %d, want 0", len(m.Contributes.Surfaces))
	}
	if domains := plugincontract.NetDomains(m.Scopes); len(domains) != 0 {
		t.Fatalf("net: scopes = %v, want none", domains)
	}
	if m.Config.Len() != 0 {
		t.Fatalf("config fields = %d, want 0: the install token is not a config secret", m.Config.Len())
	}
}

// Least privilege: the gateway reads task state; it writes nothing and reads no
// issue, comment or member content. A wider scope needs its own review.
func TestSteelBrowserManifestScopesAreReadOnlyAndMinimal(t *testing.T) {
	m := loadSteelBrowserBundle(t).Manifest
	want := []string{plugincontract.ScopeTasksRead}
	if strings.Join(m.Scopes, ",") != strings.Join(want, ",") {
		t.Fatalf("scopes = %v, want %v", m.Scopes, want)
	}
}

func TestSteelBrowserShipsItsAgentSkill(t *testing.T) {
	bundle := loadSteelBrowserBundle(t)
	if len(bundle.Manifest.Contributes.Resources) != 1 {
		t.Fatalf("resources = %d, want exactly one skill", len(bundle.Manifest.Contributes.Resources))
	}
	resource := bundle.Manifest.Contributes.Resources[0]
	if resource.Type != plugincontract.ResourceSkill || resource.Key != "steel-browser" {
		t.Fatalf("resource = %+v, want skill steel-browser", resource)
	}
	content, ok := bundle.File(resource.Entry)
	if !ok {
		t.Fatalf("skill file %s is not in the bundle", resource.Entry)
	}
	text := string(content)
	// The skill must carry the rules an agent cannot infer: never Steel
	// /context, never a credential, lease first.
	for _, must := range []string{"multica browser lease", "/context", "never type", "credential"} {
		if !strings.Contains(strings.ToLower(text), strings.ToLower(must)) {
			t.Fatalf("SKILL.md does not mention %q", must)
		}
	}
}

// The provisioning runbook is part of the deliverable (F0): it must cover the
// admin install, the token-to-secret step and the approval-gated flag change.
func TestSteelBrowserRunbookCoversProvisioning(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(steelBrowserDir(), "RUNBOOK.md"))
	if err != nil {
		t.Fatalf("runbook missing: %v", err)
	}
	text := string(raw)
	for _, must := range []string{
		"plugins_v1",
		"/api/config",
		"workspace admin",
		"install token",
		"mpi_",
		"gateway secret",
		"Cheese's approval",
		"Do not apply",
	} {
		if !strings.Contains(text, must) {
			t.Errorf("RUNBOOK.md does not contain %q", must)
		}
	}
	// A secret value must never be committed.
	if strings.Contains(text, "mpi_") {
		for _, line := range strings.Split(text, "\n") {
			if i := strings.Index(line, "mpi_"); i >= 0 {
				rest := line[i+len("mpi_"):]
				if len(rest) >= 20 && !strings.ContainsAny(rest[:1], " `*.,)") && !strings.HasPrefix(rest, "…") {
					t.Errorf("runbook line looks like it holds a token value: %q", line)
				}
			}
		}
	}
}
