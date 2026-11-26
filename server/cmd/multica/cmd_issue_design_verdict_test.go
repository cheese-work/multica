package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const designVerdictIssue = "11111111-1111-4111-8111-111111111111"

// designVerdictServer answers the issue lookup and records the verdict POST.
func designVerdictServer(t *testing.T, status int, reply string, got *map[string]any) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/issues/CHE-7090" {
			json.NewEncoder(w).Encode(map[string]string{"id": designVerdictIssue})
			return
		}
		if r.Method != "POST" || r.URL.Path != "/api/issues/"+designVerdictIssue+"/design-verdicts" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got != nil {
			if err := json.NewDecoder(r.Body).Decode(got); err != nil {
				t.Error(err)
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(server.Close)
	t.Setenv("MULTICA_SERVER_URL", server.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
}

func runDesignVerdict(t *testing.T, flags map[string]string, args ...string) (string, error) {
	t.Helper()
	cmd, _, err := rootCmd.Find([]string{"issue", "design-verdict"})
	if err != nil || cmd.Name() != "design-verdict" {
		t.Fatal("issue design-verdict is unavailable")
	}
	for _, name := range []string{"attestation", "verdict", "findings-file"} {
		_ = cmd.Flags().Set(name, "")
	}
	for key, value := range flags {
		if err := cmd.Flags().Set(key, value); err != nil {
			t.Fatal(err)
		}
	}
	return captureStdout(t, func() error { return cmd.RunE(cmd, args) })
}

func TestIssueDesignVerdictPass(t *testing.T) {
	var got map[string]any
	designVerdictServer(t, http.StatusCreated, `{"id":"v1","attestation_id":"att-1","verdict":"PASS"}`, &got)
	out, err := runDesignVerdict(t, map[string]string{"attestation": "att-1", "verdict": "PASS"}, "CHE-7090")
	if err != nil || !json.Valid([]byte(out)) {
		t.Fatalf("design-verdict: %q, %v", out, err)
	}
	if got["attestation_id"] != "att-1" || got["verdict"] != "PASS" {
		t.Errorf("wrong body: %v", got)
	}
}

func TestIssueDesignVerdictChangesSendsFindingsFile(t *testing.T) {
	var got map[string]any
	designVerdictServer(t, http.StatusCreated, `{"id":"v2"}`, &got)
	path := filepath.Join(t.TempDir(), "findings.json")
	if err := os.WriteFile(path, []byte(`[{"severity":"P1","summary":"frame 3 overlaps the nav"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runDesignVerdict(t, map[string]string{"attestation": "att-1", "verdict": "CHANGES", "findings-file": path}, "CHE-7090"); err != nil {
		t.Fatal(err)
	}
	findings, _ := got["findings"].([]any)
	if got["verdict"] != "CHANGES" || len(findings) != 1 {
		t.Fatalf("wrong body: %v", got)
	}
	if first, _ := findings[0].(map[string]any); first["severity"] != "P1" || first["summary"] != "frame 3 overlaps the nav" {
		t.Errorf("wrong finding: %v", findings[0])
	}
}

func TestIssueDesignVerdictRejectsBadInputBeforeTheServer(t *testing.T) {
	designVerdictServer(t, http.StatusCreated, `{}`, nil)
	badFile := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(badFile, []byte(`{"severity":"P1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		flags map[string]string
		args  []string
		want  string
	}{
		"no attestation":    {map[string]string{"verdict": "PASS"}, []string{"CHE-7090"}, "--attestation"},
		"no verdict":        {map[string]string{"attestation": "att-1"}, []string{"CHE-7090"}, "--verdict"},
		"verdict not enum":  {map[string]string{"attestation": "att-1", "verdict": "MAYBE"}, []string{"CHE-7090"}, "PASS or CHANGES"},
		"findings not list": {map[string]string{"attestation": "att-1", "verdict": "CHANGES", "findings-file": badFile}, []string{"CHE-7090"}, "findings"},
		"findings missing":  {map[string]string{"attestation": "att-1", "verdict": "CHANGES", "findings-file": filepath.Join(t.TempDir(), "none.json")}, []string{"CHE-7090"}, "findings"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := runDesignVerdict(t, c.flags, c.args...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestIssueDesignVerdictSurfacesTheRefusalCode(t *testing.T) {
	designVerdictServer(t, http.StatusConflict, `{"error":"the reviewer shares the author's lab","code":"design_review_same_lab"}`, nil)
	_, err := runDesignVerdict(t, map[string]string{"attestation": "att-1", "verdict": "PASS"}, "CHE-7090")
	if err == nil || !strings.Contains(err.Error(), "design_review_same_lab") {
		t.Fatalf("want the refusal code in the error, got %v", err)
	}
}
