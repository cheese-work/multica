package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/multica-ai/multica/server/internal/guard"
)

// writeGuardPolicy publishes the workspace's stored guard policy
// (settings.guard_policy, CHE-5204) to the run: a 0600 file in taskTempDir,
// outside the working tree, plus its SHA-256, in agentEnv for the guard to
// verify. No stored policy leaves agentEnv alone, so the guard keeps its
// compiled default. A policy the guard would refuse is not written.
func writeGuardPolicy(agentEnv map[string]string, taskTempDir string, settings []byte) error {
	if len(bytes.TrimSpace(settings)) == 0 {
		return nil
	}
	var s struct {
		GuardPolicy json.RawMessage `json:"guard_policy"`
	}
	if err := json.Unmarshal(settings, &s); err != nil {
		return fmt.Errorf("guard policy: workspace settings: %w", err)
	}
	if len(s.GuardPolicy) == 0 || bytes.Equal(bytes.TrimSpace(s.GuardPolicy), []byte("null")) {
		return nil
	}
	if _, err := guard.ParsePolicy(s.GuardPolicy); err != nil {
		return err
	}
	file := filepath.Join(taskTempDir, "guard-policy.json")
	if err := os.WriteFile(file, s.GuardPolicy, 0o600); err != nil {
		return fmt.Errorf("guard policy: %w", err)
	}
	sum := sha256.Sum256(s.GuardPolicy)
	agentEnv[guard.PolicyEnv] = file
	agentEnv[guard.PolicySHAEnv] = hex.EncodeToString(sum[:])
	return nil
}
