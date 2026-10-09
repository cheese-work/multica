package credentialexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(tests *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "--owned-tool-ordinary" {
		contents, err := os.ReadFile(os.Args[2])
		if err != nil || string(contents) != "owned-unlimited" || os.Getenv("OPENAI_API_KEY") != "owned-unlimited" {
			os.Exit(2)
		}
		fmt.Print("ordinary access")
		os.Exit(0)
	}
	if len(os.Args) == 3 && os.Args[1] == "--owned-tool-isolated" {
		if _, err := os.Stat(os.Args[2]); !os.IsNotExist(err) {
			os.Exit(3)
		}
		if os.Getenv("OPENAI_API_KEY") != NativePlaceholder || os.Getenv("ANTHROPIC_API_KEY") != NativePlaceholder || os.Getenv("MULTICA_TOKEN") != "" {
			os.Exit(4)
		}
		if file, err := os.OpenFile(toolBin+"/owned-tool", os.O_WRONLY, 0); err == nil {
			_ = file.Close()
			os.Exit(5)
		}
		fmt.Print("isolated tool " + os.Getenv("MULTICA_TASK_ID"))
		os.Exit(0)
	}
	os.Exit(tests.Run())
}

func ownedToolSpec(test *testing.T) (Spec, string) {
	test.Helper()
	helper, err := os.Executable()
	if err != nil {
		test.Fatal(err)
	}
	contents, err := os.ReadFile(helper)
	if err != nil {
		test.Fatal(err)
	}
	root := test.TempDir()
	native, tool := filepath.Join(root, "owned-native"), filepath.Join(root, "owned-tool")
	for _, path := range []string{native, tool} {
		if err := os.WriteFile(path, contents, 0700); err != nil {
			test.Fatal(err)
		}
	}
	spec := Spec{
		Root:             filepath.Join(root, "private"),
		Binding:          Binding{TaskID: "00000000-0000-4000-8000-000000000001", OwnerID: "00000000-0000-4000-8000-000000000002", WorkspaceID: "00000000-0000-4000-8000-000000000003"},
		Provider:         "codex",
		Executable:       native,
		HelperExecutable: helper,
		ToolExecutables:  map[string]string{"owned-tool": tool},
	}
	return spec, root
}

func TestCredentialToolSnapshots(test *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		test.Run(provider, func(test *testing.T) {
			spec, root := ownedToolSpec(test)
			spec.Provider = provider
			sentinel := filepath.Join(root, "unlimited-auth")
			if err := os.WriteFile(sentinel, []byte("owned-unlimited"), 0600); err != nil {
				test.Fatal(err)
			}
			ordinary := exec.Command(spec.ToolExecutables["owned-tool"], "--owned-tool-ordinary", sentinel)
			ordinary.Env = []string{"OPENAI_API_KEY=owned-unlimited"}
			if output, err := ordinary.CombinedOutput(); err != nil || string(output) != "ordinary access" {
				test.Fatal("ordinary negative control failed", string(output), err)
			}
			boundary, err := Prepare(context.Background(), spec)
			if err != nil {
				test.Fatal(err)
			}
			defer boundary.Close()
			marker := filepath.Join(boundary.Home(), "native-state")
			if err := os.WriteFile(marker, []byte("same task"), 0600); err != nil {
				test.Fatal(err)
			}
			retry, err := Prepare(context.Background(), spec)
			if err != nil {
				test.Fatal("unchanged tool manifest refused", err)
			}
			if err := retry.Close(); err != nil {
				test.Fatal(err)
			}
			toolSource := spec.ToolExecutables["owned-tool"]
			spec.ToolExecutables["owned-tool"] = "/owned/missing-tool"
			if err := os.Remove(toolSource); err != nil {
				test.Fatal(err)
			}
			command := exec.Command("/bin/sh", "-c", `owned-tool --owned-tool-isolated "$1"`, "owned-shell", sentinel)
			command.Dir = boundary.WorkDir()
			command.Env = []string{"OPENAI_API_KEY=owned-unlimited", "MULTICA_TOKEN=owned-unlimited", "PATH=" + root}
			cleanup, err := boundary.wrap(command, false)
			if err != nil {
				test.Fatal(err)
			}
			defer cleanup()
			if output, err := command.CombinedOutput(); err != nil || string(output) != "isolated tool "+spec.Binding.TaskID {
				test.Fatal("pinned tool failed inside boundary", string(output), err)
			}
			if contents, err := os.ReadFile(marker); err != nil || string(contents) != "same task" {
				test.Fatal("tool preparation reset native state", err)
			}
		})
	}
}

func TestCredentialToolRefusal(test *testing.T) {
	spec, root := ownedToolSpec(test)
	tool := spec.ToolExecutables["owned-tool"]
	nonELF := filepath.Join(root, "owned-script")
	if err := os.WriteFile(nonELF, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		test.Fatal(err)
	}
	tooMany := make(map[string]string)
	for index := 0; index < 17; index++ {
		tooMany[fmt.Sprintf("owned-%d", index)] = tool
	}
	cases := map[string]map[string]string{
		"empty name":      {"": tool},
		"traversal":       {"../owned-tool": tool},
		"separator":       {"owned/tool": tool},
		"reserved shell":  {"sh": tool},
		"reserved probe":  {"true": tool},
		"provider alias":  {"claude": tool},
		"other provider":  {"codex": tool},
		"helper alias":    {"multica-helper": tool},
		"long name":       {strings.Repeat("a", 65): tool},
		"empty source":    {"owned-tool": ""},
		"relative source": {"owned-tool": "owned-tool"},
		"missing source":  {"owned-tool": filepath.Join(root, "missing")},
		"unclean source":  {"owned-tool": root + "/../owned-tool"},
		"script source":   {"owned-tool": nonELF},
		"too many":        tooMany,
	}
	for name, tools := range cases {
		test.Run(name, func(test *testing.T) {
			candidate := spec
			candidate.Root = filepath.Join(test.TempDir(), "private")
			candidate.ToolExecutables = tools
			boundary, err := Prepare(context.Background(), candidate)
			if boundary != nil {
				_ = boundary.Close()
			}
			if !errors.Is(err, ErrUnavailable) {
				test.Fatal("unsupported tool manifest admitted", err)
			}
			if _, err := os.Stat(candidate.Root); !os.IsNotExist(err) {
				test.Fatal("unsupported tools created private task state", err)
			}
		})
	}
}

func TestCredentialToolManifestBinding(test *testing.T) {
	spec, root := ownedToolSpec(test)
	boundary, err := Prepare(context.Background(), spec)
	if err != nil {
		test.Fatal(err)
	}
	defer boundary.Close()
	marker := filepath.Join(boundary.Home(), "retained-state")
	if err := os.WriteFile(marker, []byte("retained"), 0600); err != nil {
		test.Fatal(err)
	}
	bindingPath := filepath.Join(boundary.state, "binding.json")
	originalBinding, err := os.ReadFile(bindingPath)
	if err != nil {
		test.Fatal(err)
	}
	for _, tools := range []map[string]string{nil, {"renamed-tool": spec.ToolExecutables["owned-tool"]}, {"owned-tool": spec.Executable}} {
		candidate := spec
		candidate.ToolExecutables = tools
		if changed, err := Prepare(context.Background(), candidate); !errors.Is(err, ErrUnavailable) {
			if changed != nil {
				_ = changed.Close()
			}
			test.Fatal("same-task tool manifest change admitted", err)
		}
	}
	tool := spec.ToolExecutables["owned-tool"]
	contents, err := os.ReadFile(tool)
	if err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(tool, append(contents, 0), 0700); err != nil {
		test.Fatal(err)
	}
	if changed, err := Prepare(context.Background(), spec); !errors.Is(err, ErrUnavailable) {
		if changed != nil {
			_ = changed.Close()
		}
		test.Fatal("same-task tool bytes changed", err)
	}
	if current, err := os.ReadFile(bindingPath); err != nil || string(current) != string(originalBinding) {
		test.Fatal("refused tool change overwrote binding", err)
	}
	if current, err := os.ReadFile(marker); err != nil || string(current) != "retained" {
		test.Fatal("refused tool change reset native state", err)
	}
	legacy := spec
	legacy.Root = filepath.Join(root, "without-tools")
	legacy.ToolExecutables = nil
	plain, err := Prepare(context.Background(), legacy)
	if err != nil {
		test.Fatal(err)
	}
	defer plain.Close()
	if plain.Environment()["PATH"] != "/bin" {
		test.Fatal("default boundary imported a tool path")
	}
	if contents, err := os.ReadFile(filepath.Join(plain.state, "binding.json")); err != nil || strings.Contains(string(contents), "ToolExecutables") {
		test.Fatal("default boundary identity changed", err)
	}
}
