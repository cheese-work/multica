package handler

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/taskassert"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// Test-only seed (the one in internal/taskassert/testdata/vector.json).
const (
	testAssertionKey = "k1:AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	testAssertionPub = "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="
)

func assertionKeys() map[string]ed25519.PublicKey {
	pub, _ := base64.StdEncoding.DecodeString(testAssertionPub)
	return map[string]ed25519.PublicKey{"k1": pub}
}

// issueTaskFixture queues one issue task on a fresh runtime/agent and project.
func issueTaskFixture(t *testing.T, suffix ...string) (taskID, agentID, runtimeID, issueID, projectID string) {
	t.Helper()
	name := "assertion " + t.Name() + strings.Join(suffix, "")
	runtimeID = dbfx.Runtime(t, name, testutil.Cols{"provider": "claude"})
	agentID = dbfx.Agent(t, name, runtimeID)
	projectID = dbfx.Project(t, "assertion project")
	issueID = dbfx.Issue(t, "assertion issue", testutil.Cols{"project_id": projectID})
	taskID = dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "queued"})
	return
}

// claimRaw runs one claim through the single-runtime endpoint.
func claimRaw(t *testing.T, runtimeID string) (task map[string]any, raw string) {
	t.Helper()
	req := withURLParam(newOwnerDaemonRequest(http.MethodPost, "/api/daemon/runtimes/"+runtimeID+"/claim", nil), "runtimeId", runtimeID)
	w := testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK)
	var resp struct {
		Task map[string]any `json:"task"`
	}
	w.JSON(&resp)
	if resp.Task == nil {
		t.Fatal("no task claimed")
	}
	return resp.Task, w.Text()
}

// claimBatch runs one claim through the batch endpoint.
func claimBatch(t *testing.T, runtimeID string) map[string]any {
	t.Helper()
	resp := testutil.Decode[struct {
		Tasks []map[string]any `json:"tasks"`
	}](t, testHandler.ClaimTasksByRuntime, batchClaimRequest(testWorkspaceID, []string{runtimeID}, 1, ""), http.StatusOK)
	if len(resp.Tasks) != 1 {
		t.Fatalf("batch claimed %d tasks, want 1", len(resp.Tasks))
	}
	return resp.Tasks[0]
}

func TestDoopTaskAssertion_ClaimsMatchTaskRow(t *testing.T) {
	requireDB(t)
	t.Setenv("MULTICA_TASK_ASSERTION_SIGNING_KEY", testAssertionKey)
	claims := map[string]func(t *testing.T, rt string) map[string]any{
		"single": func(t *testing.T, rt string) map[string]any { c, _ := claimRaw(t, rt); return c },
		"batch":  claimBatch,
	}
	for name, claim := range claims {
		t.Run(name, func(t *testing.T) {
			taskID, agentID, runtimeID, issueID, projectID := issueTaskFixture(t)
			task := claim(t, runtimeID)
			token, _ := task["doop_task_assertion"].(string)
			got, err := taskassert.Verify(token, assertionKeys())
			if err != nil {
				t.Fatalf("assertion %q does not verify: %v", token, err)
			}
			var storedModel *string
			dbfx.QueryRow(t, `SELECT claimed_model FROM agent_task_queue WHERE id = $1`, taskID).Scan(&storedModel)
			want := taskassert.Claims{
				V: 1, Kid: "k1", Aud: "doop", Wid: testWorkspaceID, Tid: taskID, Aid: agentID,
				Iid: issueID, Pid: projectID, Prv: "claude", Mdl: "", Iat: got.Iat, Exp: got.Iat + 86400,
			}
			if storedModel != nil {
				want.Mdl = *storedModel
			}
			if got != want {
				t.Fatalf("claims\n got %+v\nwant %+v", got, want)
			}
			// Nothing is stored: the assertion is not in the DB.
			var n int
			dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE id = $1 AND to_jsonb(agent_task_queue)::text LIKE '%mta1.%'`, taskID).Scan(&n)
			if n != 0 {
				t.Fatal("assertion was stored on the task row")
			}
		})
	}
}

func TestDoopTaskAssertion_ChatTaskHasEmptyIssue(t *testing.T) {
	requireDB(t)
	t.Setenv("MULTICA_TASK_ASSERTION_SIGNING_KEY", testAssertionKey)
	agentID, sessionID, runtimeID, daemonID := presetAgent(t, "claude", "claude-opus-5-5", "high")
	sendDirectChat(t, context.Background(), agentID, sessionID, "hello")
	claimed := claimForkTask(t, runtimeID, daemonID)
	got, err := taskassert.Verify(claimed.DoopTaskAssertion, assertionKeys())
	if err != nil {
		t.Fatalf("chat assertion does not verify: %v", err)
	}
	if got.Iid != "" || got.Pid != "" || got.Tid != claimed.ID || got.Aid != agentID || got.Mdl != "claude-opus-5-5" {
		t.Fatalf("chat claims = %+v", got)
	}
}

func TestDoopTaskAssertion_UnsetKeyLeavesClaimUnchanged(t *testing.T) {
	requireDB(t)
	t.Setenv("MULTICA_TASK_ASSERTION_SIGNING_KEY", "")
	_, _, runtimeID, _, _ := issueTaskFixture(t)
	task, raw := claimRaw(t, runtimeID)
	if strings.Contains(raw, "doop_task_assertion") {
		t.Fatalf("claim response has the field with no key set: %s", raw)
	}
	if task["auth_token"] == "" {
		t.Fatal("claim lost its auth token")
	}
}

func TestDoopTaskAssertion_MalformedKeyLogsOnceAndMintsNothing(t *testing.T) {
	requireDB(t)
	t.Setenv("MULTICA_TASK_ASSERTION_SIGNING_KEY", "k1:not-a-seed")
	assertionWarnMu.Lock()
	assertionWarnRaw = ""
	assertionWarnMu.Unlock()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)
	for i := 0; i < 2; i++ {
		_, _, runtimeID, _, _ := issueTaskFixture(t, string(rune('a'+i)))
		task, raw := claimRaw(t, runtimeID)
		if strings.Contains(raw, "doop_task_assertion") {
			t.Fatalf("claim carries an assertion from a malformed key: %s", raw)
		}
		if task["auth_token"] == "" {
			t.Fatal("claim failed with a malformed key")
		}
	}
	if n := strings.Count(logs.String(), "task assertion signing key is malformed"); n != 1 {
		t.Fatalf("logged %d times, want once:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), "not-a-seed") {
		t.Fatal("the key material was logged")
	}
}
