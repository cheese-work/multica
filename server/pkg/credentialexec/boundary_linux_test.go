package credentialexec

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialExecutionPreparationAndRefusal(test *testing.T) {
	root := test.TempDir()
	executable := filepath.Join(root, "owned-native")
	helper, err := os.Executable()
	if err != nil {
		test.Fatal(err)
	}
	data, err := os.ReadFile(helper)
	if err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(executable, data, 0700); err != nil {
		test.Fatal(err)
	}
	spec := Spec{Root: filepath.Join(root, "private"), Binding: Binding{TaskID: "00000000-0000-4000-8000-000000000001", OwnerID: "00000000-0000-4000-8000-000000000002", WorkspaceID: "00000000-0000-4000-8000-000000000003"}, Provider: "codex", Executable: executable, HelperExecutable: helper}
	boundary, err := Prepare(context.Background(), spec)
	if err != nil {
		if _, statErr := os.Stat("/usr/bin/bwrap"); os.IsNotExist(statErr) && errors.Is(err, ErrUnavailable) && strings.Contains(err.Error(), "bubblewrap missing") {
			test.Log("PASS: missing system bubblewrap refuses preparation")
			test.Skip("credential-exclusive integration NOT-RUN: system bubblewrap is unavailable")
		}
		test.Fatal(err)
	}
	defer boundary.Close()
	marker := filepath.Join(boundary.Home(), "native-state")
	if err := os.WriteFile(marker, []byte("same-task"), 0600); err != nil {
		test.Fatal(err)
	}
	retry, err := Prepare(context.Background(), spec)
	if err != nil {
		test.Fatal(err)
	}
	defer retry.Close()
	if retry.Home() != boundary.Home() {
		test.Fatal("same-task preparation changed state identity")
	}
	if state, err := os.ReadFile(marker); err != nil || string(state) != "same-task" {
		test.Fatal("preparation destroyed native state")
	}
	invalid := spec
	invalid.Binding.OwnerID = "00000000-0000-4000-8000-000000000004"
	if _, err := Prepare(context.Background(), invalid); !errors.Is(err, ErrUnavailable) {
		test.Fatalf("same-task owner rebind: %v", err)
	}
	invalid = spec
	invalid.Provider = "hermes"
	if _, err := Prepare(context.Background(), invalid); !errors.Is(err, ErrUnavailable) {
		test.Fatalf("unsupported provider: %v", err)
	}
	invalid = spec
	invalid.Binding.TaskID = "../other-task"
	if _, err := Prepare(context.Background(), invalid); !errors.Is(err, ErrUnavailable) {
		test.Fatalf("untrusted identity: %v", err)
	}
	invalid = spec
	invalid.Binding.TaskID = "00000000-0000-4000-8000-000000000005"
	unmanagedState := filepath.Join(spec.Root, invalid.Binding.TaskID)
	if err := os.MkdirAll(unmanagedState, 0700); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unmanagedState, "auth.json"), []byte("owned-unlimited"), 0600); err != nil {
		test.Fatal(err)
	}
	if _, err := Prepare(context.Background(), invalid); !errors.Is(err, ErrUnavailable) {
		test.Fatalf("unmanaged state import admitted: %v", err)
	}
	boundary.bwrap = "/owned/missing-bubblewrap"
	if err := boundary.Probe(context.Background()); !errors.Is(err, ErrUnavailable) {
		test.Fatalf("missing boundary accepted: %v", err)
	}
	if err := boundary.BindGateway(context.Background(), GatewayCredential{Binding: spec.Binding, BaseURL: "http://127.0.0.1:1", Key: "owned-scoped"}); err == nil {
		test.Fatal("missing boundary accepted gateway binding")
	}
	if boundary.server != nil {
		test.Fatal("failed boundary caused gateway side effects")
	}
	boundary.bwrap = "/usr/bin/bwrap"
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := boundary.Probe(canceled); !errors.Is(err, ErrUnavailable) {
		test.Fatalf("failed namespace validation accepted: %v", err)
	}
	if err := os.Remove(filepath.Join(boundary.Home(), ".codex")); err != nil {
		test.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(boundary.Home(), ".codex")); err != nil {
		test.Fatal(err)
	}
	if _, err := Prepare(context.Background(), spec); !errors.Is(err, ErrUnavailable) {
		test.Fatalf("native state symlink accepted: %v", err)
	}
	command := exec.Command(executable)
	command.Dir = boundary.WorkDir()
	if _, err := boundary.Wrap(command); err == nil {
		test.Fatal("unprovisioned gateway launched native process")
	}
}
