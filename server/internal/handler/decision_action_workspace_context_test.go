package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/multica-ai/multica/server/internal/decisionaction"
)

// workspace.context.set v1 (CHE-4485): the 10,000 code-point cap, the NUL
// refusal, the delegated-write proposer policy and the preview counts. The
// shared matrix in decision_action_instruction_set_test.go covers the rest:
// happy path with readback, stale base, wrong after digest, governed human
// blocks and the non-owner approver.

// setContextProposerGrant adds the agent to the delegated-write grant for
// workspace.context, or with granted=false empties the grant. The stored
// settings come back when the test ends.
func setContextProposerGrant(test *testing.T, agentID string, granted bool) {
	test.Helper()
	var previous string
	dbfx.QueryRow(test, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previous)
	dbfx.Cleanup(test, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID, previous)
	agents := []string{}
	if granted {
		var stored struct {
			Grant DelegatedWritesConfig `json:"delegated_governed_writes"`
		}
		_ = json.Unmarshal([]byte(previous), &stored)
		agents = append([]string{agentID}, stored.Grant.AllowedAgents...)
	}
	setting, err := json.Marshal(map[string]any{"delegated_governed_writes": DelegatedWritesConfig{
		AllowedAgents: agents, AllowedFields: []string{"workspace.context"}, RequiresHumanDecision: true,
	}})
	if err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE workspace SET settings = settings || $2::jsonb WHERE id = $1`, testWorkspaceID, string(setting))
}

// contextOfLength is governedText with its governed:agent region padded with a
// two-byte rune to exactly n code points; the human block stays intact.
func contextOfLength(n int) string {
	return strings.Replace(governedText, "tail", "tail"+strings.Repeat("é", n-utf8.RuneCountInString(governedText)), 1)
}

// requestContext stores live as the workspace context and asks for after.
func requestContext(test *testing.T, status int, live, after string) (instructionSetRequest, string) {
	test.Helper()
	target := instructionSetTargets()[1]
	return requestInstructionSet(test, target, status, func(id string, params map[string]any) {
		target.set(test, id, live)
		params[target.contentKey] = after
		params["base_sha256"], params["after_sha256"] = decisionaction.TextDigest(live), decisionaction.TextDigest(after)
	})
}

func TestWorkspaceContextCapCountsCodePoints(test *testing.T) {
	// "é" is two bytes: 10,000 of them pass, 10,001 do not.
	requestContext(test, http.StatusCreated, governedText, contextOfLength(workspaceContextCap))
	_, body := requestContext(test, http.StatusUnprocessableEntity, governedText, contextOfLength(workspaceContextCap+1))
	mustContain(test, body, "context_too_long")
}

func TestWorkspaceContextOversizedValueMayShrinkButNotGrow(test *testing.T) {
	live := contextOfLength(workspaceContextCap + 500)
	requestContext(test, http.StatusCreated, live, contextOfLength(workspaceContextCap+200))
	_, body := requestContext(test, http.StatusUnprocessableEntity, live, contextOfLength(workspaceContextCap+501))
	mustContain(test, body, "context_too_long")
}

func TestWorkspaceContextRefusesANulByte(test *testing.T) {
	_, body := requestContext(test, http.StatusBadRequest, governedText, strings.Replace(governedText, "tail", "ta\x00il", 1))
	mustContain(test, body, "context_has_nul")
}

func TestWorkspaceContextProposerMustHoldTheDelegatedWriteGrant(test *testing.T) {
	target := instructionSetTargets()[1]
	_, body := requestInstructionSet(test, target, http.StatusForbidden, func(string, map[string]any) {
		setContextProposerGrant(test, "", false)
	})
	mustContain(test, body, "not_permitted")
}

func TestWorkspaceContextRevokedProposerIsRefusedAtApproval(test *testing.T) {
	request, _ := requestContext(test, http.StatusCreated, governedText, editedText)
	setContextProposerGrant(test, "", false)
	mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "proposer_revoked")
	request.assertNotApplied(test, governedText)
}

func TestWorkspaceContextPreviewCarriesCounts(test *testing.T) {
	request, _ := requestContext(test, http.StatusCreated, governedText, editedText)
	shown, _ := request.preconditions(test)["preview"].(map[string]any)
	meta, _ := shown["meta"].(map[string]any)
	want := map[string]any{
		"unit": "code_points", "chars_before": float64(utf8.RuneCountInString(governedText)),
		"chars_after": float64(utf8.RuneCountInString(editedText)), "cap": float64(workspaceContextCap),
	}
	for key, value := range want {
		if meta[key] != value {
			test.Fatalf("preview meta %v: %s = %v, want %v", meta, key, meta[key], value)
		}
	}
}

func TestWorkspaceContextApprovalWritesOnlyTheContext(test *testing.T) {
	const snapshot = `SELECT (to_jsonb(workspace) - 'context' - 'updated_at')::text FROM workspace WHERE id = $1`
	request, _ := requestContext(test, http.StatusCreated, governedText, editedText)
	var before, after string
	dbfx.QueryRow(test, snapshot, testWorkspaceID).Scan(&before)
	request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
	dbfx.QueryRow(test, snapshot, testWorkspaceID).Scan(&after)
	if before != after {
		test.Fatalf("approval changed other workspace columns:\nbefore %s\nafter  %s", before, after)
	}
	if got := request.target.live(test, request.id); got != editedText {
		test.Fatalf("context %q, want %q", got, editedText)
	}
}
