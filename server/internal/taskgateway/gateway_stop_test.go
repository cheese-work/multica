package taskgateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/multica-ai/multica/server/pkg/credentialexec"
)

func TestTaskGatewayStoppedStateNeverProvisions(test *testing.T) {
	operator, binding, calls := gatewayFixture(test)
	root := test.TempDir()
	state := filepath.Join(root, binding.TaskID)
	if err := os.Mkdir(state, 0700); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "gateway-stop"), []byte("quota\n"), 0600); err != nil {
		test.Fatal(err)
	}
	spec := credentialexec.Spec{Root: root, Binding: binding, Provider: "codex", Executable: "/owned/missing-native", HelperExecutable: "/owned/missing-helper"}
	if boundary, err := operator.Prepare(context.Background(), "00000000-0000-4000-8000-000000000004", spec); boundary != nil || !errors.Is(err, ErrUnavailable) || calls.Load() != 0 {
		test.Fatal("stopped same-task state reached provisioning")
	}
	fetched := false
	if boundary, err := PrepareHandoff(context.Background(), spec, func(context.Context) (Grant, error) { fetched = true; return Grant{}, ErrUnavailable }); boundary != nil || err == nil || fetched {
		test.Fatal("stopped task requested another grant")
	}
}
