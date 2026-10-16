package taskgateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/multica-ai/multica/server/pkg/credentialexec"
)

func TestTaskGatewayFailedPreparationHasNoOperatorSideEffects(test *testing.T) {
	operator, binding, calls := gatewayFixture(test)
	spec := credentialexec.Spec{Root: filepath.Join(test.TempDir(), "private"), Binding: binding, Provider: "codex", Executable: "/owned/missing-native", HelperExecutable: "/owned/missing-helper"}
	if boundary, err := operator.Prepare(context.Background(), "00000000-0000-4000-8000-000000000004", spec); !errors.Is(err, ErrUnavailable) || boundary != nil || calls.Load() != 0 {
		test.Fatalf("failed preparation reached operator: %v calls=%d", err, calls.Load())
	}
	spec.Binding.OwnerID = "00000000-0000-4000-8000-000000000005"
	if boundary, err := operator.Prepare(context.Background(), "00000000-0000-4000-8000-000000000004", spec); !errors.Is(err, ErrUnavailable) || boundary != nil || calls.Load() != 0 {
		test.Fatalf("forged binding reached operator: %v calls=%d", err, calls.Load())
	}
}

func TestTaskGatewayPreparedHandoffAndSameTaskRetry(test *testing.T) {
	if _, err := os.Stat("/usr/bin/bwrap"); os.IsNotExist(err) {
		test.Skip("prepared handoff NOT-RUN: system bubblewrap is unavailable")
	}
	operator, binding, calls := gatewayFixture(test)
	root := test.TempDir()
	helper, err := os.Executable()
	if err != nil {
		test.Fatal(err)
	}
	executable := filepath.Join(root, "owned-native")
	binary, err := os.ReadFile(helper)
	if err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(executable, binary, 0700); err != nil {
		test.Fatal(err)
	}
	for _, provider := range []string{"claude", "codex"} {
		spec := credentialexec.Spec{Root: filepath.Join(root, provider), Binding: binding, Provider: provider, Executable: executable, HelperExecutable: helper}
		var home string
		for attempt := 0; attempt < 2; attempt++ {
			boundary, err := operator.Prepare(context.Background(), "00000000-0000-4000-8000-000000000004", spec)
			if err != nil {
				test.Fatal(err)
			}
			test.Cleanup(func() {
				if err := boundary.Close(); err != nil {
					test.Error(err)
				}
			})
			if attempt == 0 {
				home = boundary.Home()
				if err := os.WriteFile(filepath.Join(home, "owned-native-state"), []byte("same-task"), 0600); err != nil {
					test.Fatal(err)
				}
			} else if contents, err := os.ReadFile(filepath.Join(boundary.Home(), "owned-native-state")); err != nil || string(contents) != "same-task" || boundary.Home() != home {
				test.Fatal("same-task handoff reset native state")
			}
			if err := boundary.Validate(binding.TaskID, provider, executable); err != nil {
				test.Fatal(err)
			}
			if err := boundary.Close(); err != nil {
				test.Fatal(err)
			}
			if err := boundary.Validate(binding.TaskID, provider, executable); err == nil {
				test.Fatal("closed broker remained launchable")
			}
		}
	}
	if calls.Load() != 12 {
		test.Fatalf("handoff did not replay authorized operator contract: %d", calls.Load())
	}
}

func TestTaskGatewaySameTaskGrantCannotRebind(test *testing.T) {
	if _, err := os.Stat("/usr/bin/bwrap"); os.IsNotExist(err) {
		test.Skip("same-task rebind control NOT-RUN: system bubblewrap is unavailable")
	}
	operator, binding, calls := gatewayFixture(test)
	root := test.TempDir()
	helper, err := os.Executable()
	if err != nil {
		test.Fatal(err)
	}
	executable := filepath.Join(root, "owned-native")
	binary, err := os.ReadFile(helper)
	if err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(executable, binary, 0700); err != nil {
		test.Fatal(err)
	}
	spec := credentialexec.Spec{Root: filepath.Join(root, "private"), Binding: binding, Provider: "codex", Executable: executable, HelperExecutable: helper}
	boundary, err := operator.Prepare(context.Background(), "00000000-0000-4000-8000-000000000004", spec)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if err := boundary.Close(); err != nil {
			test.Error(err)
		}
	})
	if err := boundary.Close(); err != nil {
		test.Fatal(err)
	}
	home := boundary.Home()
	if err := os.WriteFile(filepath.Join(home, "native-state"), []byte("retained"), 0600); err != nil {
		test.Fatal(err)
	}
	changed, _, _ := gatewayFixtureResponse(test, func(route, response string) string { return response })
	if rebound, err := changed.Prepare(context.Background(), "00000000-0000-4000-8000-000000000004", spec); !errors.Is(err, ErrUnavailable) || rebound != nil {
		if rebound != nil {
			_ = rebound.Close()
		}
		test.Fatalf("changed same-task gateway origin admitted: %v", err)
	}
	if contents, err := os.ReadFile(filepath.Join(home, "native-state")); err != nil || string(contents) != "retained" {
		test.Fatal("refusal deleted native state")
	}
	if calls.Load() != 3 {
		test.Fatal("old gateway received retry or fallback traffic")
	}
}
