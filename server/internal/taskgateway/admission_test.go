package taskgateway

import (
	"slices"
	"testing"

	"github.com/multica-ai/multica/server/pkg/credentialexec"
)

func TestTaskGatewayAdmission(test *testing.T) {
	operator, binding, calls := gatewayFixture(test)
	runtimeID := "00000000-0000-4000-8000-000000000004"
	for _, candidate := range []struct {
		name         string
		runtime      string
		provider     string
		binding      credentialexec.Binding
		capabilities []string
		required     bool
		refused      bool
	}{
		{"trusted", runtimeID, "claude", binding, []string{Capability}, true, false},
		{"codex", runtimeID, "codex", binding, []string{Capability}, true, false},
		{"old daemon", runtimeID, "codex", binding, nil, true, true},
		{"unrelated capability", runtimeID, "codex", binding, []string{"task-gateway-v10"}, true, true},
		{"unsupported provider", runtimeID, "custom", binding, []string{Capability}, true, true},
		{"moved protected task", "00000000-0000-4000-8000-000000000005", "codex", binding, []string{Capability}, true, true},
		{"forged owner", runtimeID, "codex", credentialexec.Binding{TaskID: binding.TaskID, WorkspaceID: binding.WorkspaceID, OwnerID: runtimeID}, []string{Capability}, true, true},
		{"forged workspace", runtimeID, "codex", credentialexec.Binding{TaskID: binding.TaskID, WorkspaceID: runtimeID, OwnerID: binding.OwnerID}, []string{Capability}, true, true},
		{"different task on protected runtime", runtimeID, "codex", credentialexec.Binding{TaskID: runtimeID, WorkspaceID: binding.WorkspaceID, OwnerID: binding.OwnerID}, []string{Capability}, true, true},
		{"invalid binding", runtimeID, "codex", credentialexec.Binding{}, []string{Capability}, true, true},
		{"unmanaged", "00000000-0000-4000-8000-000000000005", "custom", credentialexec.Binding{TaskID: runtimeID}, nil, false, false},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			original := slices.Clone(candidate.capabilities)
			required, err := operator.Admit(candidate.runtime, candidate.provider, candidate.binding, candidate.capabilities)
			if required != candidate.required || (err != nil) != candidate.refused {
				test.Fatalf("required=%v refused=%v", required, err != nil)
			}
			if !slices.Equal(candidate.capabilities, original) {
				test.Fatal("capabilities mutated")
			}
		})
	}
	if calls.Load() != 0 {
		test.Fatal("claim admission called the operator before OS preparation")
	}
	var disabled *Provisioner
	if required, err := disabled.Admit(runtimeID, "claude", binding, nil); required || err != nil {
		test.Fatal("disabled policy changed unmanaged admission")
	}
}
