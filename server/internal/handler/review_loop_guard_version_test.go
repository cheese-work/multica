package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// W3 of CHE-9942: a refused review verdict names the CLI form, and a round-2
// refusal says when the client CLI predates the @revision form (PR #2139).

func TestCLIOutdatedForReviewFinding(t *testing.T) {
	for name, tc := range map[string]struct {
		version        string
		outdated, know bool
	}{
		"equal":              {"0.10.0-20261125-1520", false, true},
		"older stamp":        {"0.10.0-20261125-1519", true, true},
		"newer stamp":        {"0.10.0-20261125-1812", false, true},
		"older minor":        {"0.8.0-20261124-2141", true, true},
		"newer minor":        {"0.11.0-20261101-0000", false, true},
		"older minor, newer": {"0.9.9-20261201-0000", true, true},
		"v prefix":           {"v0.10.0-20261125-1520", false, true},
		"build metadata":     {"0.10.0-20261125-1520+abc", false, true},
		"dev":                {"dev", false, false},
		"git describe":       {"v0.10.0-5-gabcdef1", false, false},
		"plain release":      {"0.10.0", false, false},
		"empty":              {"", false, false},
	} {
		outdated, known := cliOutdatedForReviewFinding(tc.version)
		if outdated != tc.outdated || known != tc.know {
			t.Errorf("%s: %q = (outdated %v, known %v), want (%v, %v)", name, tc.version, outdated, known, tc.outdated, tc.know)
		}
	}
}

const (
	cliFormRound2 = `--finding "P1@<category>@revision:<summary>"`
	cliFormPlain  = `--finding "P1@<category>:<summary>"`
)

// round2Refusal drives the CHE-9307 failing case with a client version header.
func round2Refusal(t *testing.T, clientVersion string) map[string]any {
	t.Helper()
	l := newReviewLoop(t)
	l.verdict(t, "in_progress", categorized("P1", "missing_rollback", "no rollback step")).Want(http.StatusOK)
	l.backToReview(t)
	req := withURLParam(newRequest("PUT", "/api/issues/"+l.issueID, map[string]any{
		"status": "in_progress",
		"review_findings": []map[string]any{
			categorized("P1", "missing_rollback", "no rollback step"),
			categorized("P1", "scope_creep", "adds an unrelated endpoint"),
		},
	}), "id", l.issueID)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", l.agentID)
	req.Header.Set("X-Task-ID", l.run)
	// The handler is called directly, so the ClientMetadata middleware does not run.
	req = req.WithContext(middleware.SetClientMetadata(req.Context(), "cli", clientVersion, ""))
	return testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusBadRequest).Map()
}

func TestReviewLoop_RoundTwoRefusalFlagsOutdatedCLI(t *testing.T) {
	body := round2Refusal(t, "0.8.0-20261124-2141")
	if body["code"] != "review_round_new_blocker" {
		t.Fatalf("refusal = %v", body)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, cliFormRound2) {
		t.Errorf("error lacks the CLI form: %q", msg)
	}
	if !strings.Contains(msg, "your CLI 0.8.0-20261124-2141 is older than 0.10.0-20261125-1520; update the multica CLI") {
		t.Errorf("error lacks the upgrade line: %q", msg)
	}
	if body["client_outdated"] != true || body["client_version"] != "0.8.0-20261124-2141" || body["min_client_version"] != "0.10.0-20261125-1520" || body["cli_form"] != cliFormRound2 {
		t.Errorf("json fields = %v", body)
	}
}

func TestReviewLoop_RoundTwoRefusalCurrentCLIHasNoUpgradeLine(t *testing.T) {
	body := round2Refusal(t, "0.10.0-20261125-1812")
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, cliFormRound2) || strings.Contains(msg, "update the multica CLI") {
		t.Errorf("error = %q", msg)
	}
	if body["client_outdated"] != false || body["cli_form"] != cliFormRound2 {
		t.Errorf("json fields = %v", body)
	}
}

func TestReviewLoop_RoundTwoRefusalUnknownCLIIsNeverOutdated(t *testing.T) {
	for _, v := range []string{"dev", ""} {
		body := round2Refusal(t, v)
		msg, _ := body["error"].(string)
		if !strings.Contains(msg, cliFormRound2) || strings.Contains(msg, "update the multica CLI") {
			t.Errorf("%q: error = %q", v, msg)
		}
		for _, k := range []string{"client_outdated", "client_version", "min_client_version"} {
			if _, ok := body[k]; ok {
				t.Errorf("%q: json has %s: %v", v, k, body)
			}
		}
		if body["cli_form"] != cliFormRound2 {
			t.Errorf("%q: cli_form = %v", v, body["cli_form"])
		}
	}
}

func TestReviewLoop_FindingsAndCategoryRefusalsNameTheCLIForm(t *testing.T) {
	l := newReviewLoop(t)
	body := l.verdict(t, "in_progress", finding("P2")).Want(http.StatusBadRequest).Map()
	if msg, _ := body["error"].(string); body["code"] != "review_verdict_findings_required" || !strings.Contains(msg, cliFormPlain) || body["cli_form"] != cliFormPlain {
		t.Errorf("findings refusal = %v", body)
	}
	if _, ok := body["client_outdated"]; ok {
		t.Errorf("findings refusal has client_outdated: %v", body)
	}

	setCategoryMode(t, "required")
	body = l.verdict(t, "in_progress", map[string]any{"severity": "P1", "summary": "x"}).Want(http.StatusBadRequest).Map()
	if msg, _ := body["error"].(string); body["code"] != "review_finding_category_required" || !strings.Contains(msg, cliFormPlain) || body["cli_form"] != cliFormPlain {
		t.Errorf("category refusal = %v", body)
	}
}
