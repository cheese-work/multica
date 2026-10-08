package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/multica-ai/multica/server/pkg/credentialexec"
)

func TestTaskGatewayPinnedSkillRefusals(test *testing.T) {
	for _, kind := range []string{"changed-hash", "wrong-skill", "missing-bundle", "extra-bundle", "invalid-json", "unknown-field", "trailing-json", "unsafe-file", "duplicate-file", "oversized-file", "oversized-response", "429", "redirect", "unavailable"} {
		test.Run(kind, func(test *testing.T) {
			task, binding := ownedTaskGatewayClaim()
			bundle := makeResolvableSkillBundle("owned-skill")
			switch kind {
			case "unsafe-file":
				bundle.Files[0].Path = "../escape"
			case "duplicate-file":
				bundle.Files = append(bundle.Files, bundle.Files[0])
			case "oversized-file":
				bundle.Content = strings.Repeat("x", (1<<20)+1)
			}
			manifest := skillRefFromBundle(bundle)
			bundle.Hash, bundle.SizeBytes = manifest.Hash, manifest.SizeBytes
			task.Agent.SkillRefs = []SkillRefData{skillRefFromBundle(bundle)}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				if request.Method != http.MethodPost || request.URL.Path != "/api/daemon/runtimes/"+task.RuntimeID+"/tasks/"+task.ID+"/skill-bundles/resolve" || request.Header.Get("Authorization") != "Bearer "+task.TaskGatewayDaemonToken {
					test.Error("resolution used a fallback credential or wrong task route")
				}
				var body struct {
					Skills []SkillRefData `json:"skills"`
				}
				if json.NewDecoder(request.Body).Decode(&body) != nil || len(body.Skills) != 1 || body.Skills[0].Hash != task.Agent.SkillRefs[0].Hash {
					test.Error("resolution dropped the claim's pinned manifest")
				}
				bundles := []SkillData{bundle}
				switch kind {
				case "changed-hash":
					bundles[0] = makeResolvableSkillBundleWith(bundle.ID, "changed claim content", "changed file")
				case "wrong-skill":
					bundles[0] = makeResolvableSkillBundle("other-skill")
				case "missing-bundle":
					bundles = nil
				case "extra-bundle":
					bundles = append(bundles, bundle)
				case "invalid-json":
					_, _ = writer.Write([]byte("owned-secret invalid response"))
					return
				case "unknown-field":
					_ = json.NewEncoder(writer).Encode(map[string]any{"bundles": bundles, "fallback": "owned-secret"})
					return
				case "trailing-json":
					_ = json.NewEncoder(writer).Encode(map[string]any{"bundles": bundles})
					_, _ = writer.Write([]byte(`{"fallback":"owned-secret"}`))
					return
				case "oversized-response":
					_, _ = writer.Write([]byte(strings.Repeat(" ", (8<<20)+1)))
					return
				case "429":
					writer.WriteHeader(http.StatusTooManyRequests)
					_, _ = writer.Write([]byte("owned-secret"))
					return
				case "redirect":
					writer.Header().Set("Location", "/unlimited-fallback")
					writer.WriteHeader(http.StatusFound)
					return
				case "unavailable":
					writer.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				_ = json.NewEncoder(writer).Encode(map[string]any{"bundles": bundles})
			}))
			defer server.Close()
			client := NewClient(server.URL)
			client.SetToken("mdt_owned-unlimited-fallback")
			spec := credentialexec.Spec{Root: test.TempDir(), Binding: binding, Provider: "codex", Executable: "/owned/missing-native"}
			boundary, err := client.PrepareTaskGateway(context.Background(), task, spec)
			if err == nil || boundary != nil || calls.Load() != 1 || strings.Contains(err.Error(), "owned-secret") {
				test.Fatalf("pinned resolution did not refuse without retry, grant or secret echo: calls=%d error=%v", calls.Load(), err)
			}
			if len(task.Agent.SkillRefs) != 1 || len(task.Agent.Skills) != 0 {
				test.Fatal("failed resolution mutated the authenticated claim")
			}
		})
	}
}

func TestTaskGatewayInvalidSkillClaimsNeverResolve(test *testing.T) {
	for _, kind := range []string{"unsupported-source", "missing-hash", "negative-size", "oversized-size", "negative-count", "oversized-count", "excessive-refs", "forged-owner", "forged-agent", "unmanaged", "owner-token", "unsafe-origin"} {
		test.Run(kind, func(test *testing.T) {
			task, binding := ownedTaskGatewayClaim()
			bundle := makeResolvableSkillBundle("owned-skill")
			ref := skillRefFromBundle(bundle)
			switch kind {
			case "unsupported-source":
				ref.Source = "host-cache"
			case "missing-hash":
				ref.Hash = ""
			case "negative-size":
				ref.SizeBytes = -1
			case "oversized-size":
				ref.SizeBytes = (8 << 20) + 1
			case "negative-count":
				ref.FileCount = -1
			case "oversized-count":
				ref.FileCount = 125
			case "forged-owner":
				binding.OwnerID = "00000000-0000-4000-8000-000000000099"
			case "forged-agent":
				task.Agent.ID = "forged"
			case "unmanaged":
				task.RequireCredentialIsolation = false
			case "owner-token":
				task.TaskGatewayDaemonToken = "mul_owned-owner-token"
			}
			task.Agent.SkillRefs = []SkillRefData{ref}
			if kind == "excessive-refs" {
				task.Agent.SkillRefs = make([]SkillRefData, 126)
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
			defer server.Close()
			client := NewClient(server.URL)
			if kind == "unsafe-origin" {
				client.baseURL += "?credential=owned-secret"
			}
			boundary, err := client.PrepareTaskGateway(context.Background(), task, credentialexec.Spec{Root: test.TempDir(), Binding: binding, Provider: "codex", Executable: "/owned/missing-native"})
			if err == nil || boundary != nil || calls.Load() != 0 {
				test.Fatal("invalid claim reached skill or grant delivery")
			}
		})
	}
}

func TestTaskGatewayPinnedSkillsStageBeforeGrant(test *testing.T) {
	if _, err := os.Stat("/usr/bin/bwrap"); os.IsNotExist(err) {
		test.Skip("pinned skill staging NOT-RUN: system bubblewrap unavailable")
	}
	for _, provider := range []string{"claude", "codex"} {
		test.Run(provider, func(test *testing.T) {
			task, binding := ownedTaskGatewayClaim()
			bundle := makeResolvableSkillBundle("owned-skill")
			task.Agent.SkillRefs = []SkillRefData{skillRefFromBundle(bundle)}
			resolved := task
			resolvedAgent := *task.Agent
			resolved.Agent = &resolvedAgent
			resolved.Agent.SkillRefs, resolved.Agent.Skills = nil, []SkillData{bundle}
			root := test.TempDir()
			helper, err := os.Executable()
			if err != nil {
				test.Fatal(err)
			}
			contents, err := os.ReadFile(helper)
			if err != nil {
				test.Fatal(err)
			}
			executable := filepath.Join(root, "owned-native")
			if err := os.WriteFile(executable, contents, 0500); err != nil {
				test.Fatal(err)
			}
			spec := credentialexec.Spec{Root: filepath.Join(root, "private"), Binding: binding, Provider: provider, Executable: executable, HelperExecutable: helper}
			var resolves, grants atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if strings.HasSuffix(request.URL.Path, "/skill-bundles/resolve") {
					resolves.Add(1)
					_ = json.NewEncoder(writer).Encode(map[string]any{"bundles": []SkillData{bundle}})
					return
				}
				grants.Add(1)
				for name, expected := range map[string]string{"prompt.md": BuildPrompt(resolved, provider), "skills/owned-skill/SKILL.md": bundle.Content, "skills/owned-skill/rules.md": bundle.Files[0].Content} {
					actual, err := os.ReadFile(filepath.Join(spec.Root, task.ID, "workdir", "multica-input", name))
					if err != nil || string(actual) != expected {
						test.Error("grant preceded exact pinned input staging")
					}
				}
				_ = json.NewEncoder(writer).Encode(map[string]any{"binding": binding, "base_url": "https://owned.example", "key": "owned-gateway-secret"})
			}))
			defer server.Close()
			client := NewClient(server.URL)
			for attempt := 0; attempt < 2; attempt++ {
				boundary, err := client.PrepareTaskGateway(context.Background(), task, spec)
				if err != nil {
					test.Fatal(err)
				}
				marker := filepath.Join(boundary.Home(), "native-state")
				if attempt == 0 {
					if err := os.WriteFile(marker, []byte("retained"), 0600); err != nil {
						test.Fatal(err)
					}
				} else if contents, err := os.ReadFile(marker); err != nil || string(contents) != "retained" {
					test.Fatal("same-task pinned resolution reset native state")
				}
				if err := boundary.Close(); err != nil {
					test.Fatal(err)
				}
			}
			if resolves.Load() != 2 || grants.Load() != 2 || len(task.Agent.SkillRefs) != 1 || len(task.Agent.Skills) != 0 {
				test.Fatal("same-task resolution changed the claim or credential request count")
			}
		})
	}
}
