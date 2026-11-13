package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/configsync"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-3284: config-sync enrollment (owner CAS) and the dry-run receipt
// doorbell. Nothing here may write a governed field.

const configSyncSettingsJSON = `{"repo":"acme/widget","default_branch":"main","manifest_path":"config/manifest.json","owner_logins":["Owner"]}`

func configSyncPatch(userID, settings, digest string) *http.Request {
	return withURLParam(newRequestAs(userID, http.MethodPatch, "/api/workspaces/"+testWorkspaceID,
		map[string]any{"config_sync": settings, "expected_before_digest": digest}), "id", testWorkspaceID)
}

func readConfigSync(t *testing.T) string {
	t.Helper()
	var v string
	err := testPool.QueryRow(context.Background(), `SELECT settings FROM workspace_config_sync WHERE workspace_id = $1`, testWorkspaceID).Scan(&v)
	if err != nil {
		return ""
	}
	return v
}

func cleanConfigSync(t *testing.T) {
	t.Helper()
	del := func() {
		testPool.Exec(context.Background(), `DELETE FROM workspace_config_sync WHERE workspace_id = $1`, testWorkspaceID)
	}
	del()
	t.Cleanup(del)
}

func TestConfigSyncOwnerCAS(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	cleanConfigSync(t)

	// Unenrolled = empty value; its digest is the digest of the empty string.
	w := httptest.NewRecorder()
	testHandler.UpdateWorkspace(w, configSyncPatch(testUserID, configSyncSettingsJSON, testDigest("")))
	if w.Code != http.StatusOK {
		t.Fatalf("owner enroll: want 200, got %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		After string `json:"after_digest"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if got := readConfigSync(t); got != configSyncSettingsJSON || out.After != testDigest(configSyncSettingsJSON) {
		t.Fatalf("stored %q, after_digest %q", got, out.After)
	}

	// A stale digest writes nothing.
	w = httptest.NewRecorder()
	testHandler.UpdateWorkspace(w, configSyncPatch(testUserID, strings.Replace(configSyncSettingsJSON, "main", "dev", 1), testDigest("")))
	if w.Code != http.StatusConflict || readConfigSync(t) != configSyncSettingsJSON {
		t.Fatalf("stale digest: want 409 and no change, got %d: %s", w.Code, w.Body.String())
	}

	// Invalid settings are refused before any write.
	w = httptest.NewRecorder()
	testHandler.UpdateWorkspace(w, configSyncPatch(testUserID, `{"repo":"nope"}`, testDigest(configSyncSettingsJSON)))
	if w.Code != http.StatusBadRequest || readConfigSync(t) != configSyncSettingsJSON {
		t.Fatalf("invalid settings: want 400 and no change, got %d: %s", w.Code, w.Body.String())
	}

	// The empty value un-enrolls.
	w = httptest.NewRecorder()
	testHandler.UpdateWorkspace(w, configSyncPatch(testUserID, "", testDigest(configSyncSettingsJSON)))
	if w.Code != http.StatusOK || readConfigSync(t) != "" {
		t.Fatalf("un-enroll: want 200 and empty, got %d: %s", w.Code, w.Body.String())
	}
}

func TestConfigSyncCASIsOwnerOnly(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	cleanConfigSync(t)
	member, hostAgent, hostTask := governedInstructionActorFixture(t, "che3284-cas-member@multica.test", "che3284-cas-host")

	cases := map[string]*http.Request{
		"plain member": configSyncPatch(member, configSyncSettingsJSON, testDigest("")),
		"agent actor":  asAgentActor(configSyncPatch(testUserID, configSyncSettingsJSON, testDigest("")), hostAgent, hostTask),
		"cloud PAT":    asCloudNodeActor(configSyncPatch(testUserID, configSyncSettingsJSON, testDigest(""))),
	}
	for name, req := range cases {
		w := httptest.NewRecorder()
		testHandler.UpdateWorkspace(w, req)
		if w.Code != http.StatusForbidden || readConfigSync(t) != "" {
			t.Errorf("%s: want 403 and no write, got %d: %s", name, w.Code, w.Body.String())
		}
	}
}

// fakeConfigSyncGitHub answers from fixed API values and records comments.
type fakeConfigSyncGitHub struct {
	pr       configsync.PullRequest
	commits  map[string]configsync.Commit
	files    map[string]string
	comments []string
}

func (f *fakeConfigSyncGitHub) PullRequest(context.Context, int) (configsync.PullRequest, error) {
	return f.pr, nil
}
func (f *fakeConfigSyncGitHub) TimelineEvents(context.Context, int) ([]string, error) {
	return nil, nil
}
func (f *fakeConfigSyncGitHub) PRCommitMessages(context.Context, int) ([]string, error) {
	return []string{"c"}, nil
}
func (f *fakeConfigSyncGitHub) Commit(_ context.Context, sha string) (configsync.Commit, error) {
	c, ok := f.commits[sha]
	if !ok {
		return configsync.Commit{}, errors.New("no commit")
	}
	return c, nil
}
func (f *fakeConfigSyncGitHub) File(_ context.Context, ref, path string) ([]byte, bool, error) {
	v, ok := f.files[ref+":"+path]
	return []byte(v), ok, nil
}
func (f *fakeConfigSyncGitHub) PostComment(_ context.Context, _ int, body string) error {
	f.comments = append(f.comments, body)
	return nil
}

type configSyncWebhookFixture struct {
	gh      *fakeConfigSyncGitHub
	secret  string
	factory int // times the GitHub reader was built
	instID  int64
}

func newConfigSyncWebhookFixture(t *testing.T, mergedBy string) *configSyncWebhookFixture {
	t.Helper()
	if testHandler == nil {
		t.Skip("database not available")
	}
	const instID int64 = 3284001
	fx := &configSyncWebhookFixture{secret: "che3284-secret", instID: instID}
	t.Setenv("GITHUB_WEBHOOK_SECRET", fx.secret)

	cleanConfigSync(t)
	dbfx.Exec(t, `INSERT INTO workspace_config_sync (workspace_id, settings) VALUES ($1, $2)`, testWorkspaceID, configSyncSettingsJSON)
	dbfx.Exec(t, `INSERT INTO github_installation (workspace_id, installation_id, account_login, account_type) VALUES ($1, $2, 'che3284', 'User')`, testWorkspaceID, instID)
	var previous *string
	dbfx.QueryRow(t, `SELECT context FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previous)
	dbfx.Exec(t, `UPDATE workspace SET context = 'old context' WHERE id = $1`, testWorkspaceID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM github_installation WHERE installation_id = $1`, instID)
		testPool.Exec(context.Background(), `DELETE FROM github_pull_request WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(context.Background(), `UPDATE workspace SET context = $1 WHERE id = $2`, previous, testWorkspaceID)
	})

	fx.gh = &fakeConfigSyncGitHub{
		pr:      configsync.PullRequest{Merged: true, MergedByLogin: mergedBy, MergedByType: "User", BaseRef: "main", MergeCommitSHA: "mrg"},
		commits: map[string]configsync.Commit{"mrg": {Parents: []string{"pre"}, Message: "m"}},
		files: map[string]string{
			"pre:config/manifest.json": `{"entries":[{"target":"workspace.context","file":"ctx.md"}]}`,
			"pre:ctx.md":               "old context",
			"mrg:ctx.md":               "new context",
		},
	}
	prevFactory, prevRun := testHandler.ConfigSyncGitHub, testHandler.configSyncRun
	testHandler.ConfigSyncGitHub = func(int64, string, string) ConfigSyncGitHub { fx.factory++; return fx.gh }
	testHandler.configSyncRun = func(f func()) { f() }
	t.Cleanup(func() { testHandler.ConfigSyncGitHub, testHandler.configSyncRun = prevFactory, prevRun })
	return fx
}

func (fx *configSyncWebhookFixture) body(claimedMerger string) map[string]any {
	b := buildMergedPRWebhookBody("NONE-1", 7, "acme", "widget", fx.instID)
	b["pull_request"].(map[string]any)["merged_by"] = map[string]any{"login": claimedMerger, "type": "User"}
	return b
}

func TestConfigSyncWebhookPostsDryRunAndWritesNothing(t *testing.T) {
	fx := newConfigSyncWebhookFixture(t, "owner")
	postSignedGitHubWebhook(t, fx.secret, fx.body("owner"), "che3284-ok")

	if len(fx.gh.comments) != 1 || !strings.Contains(fx.gh.comments[0], "would apply") || !strings.Contains(fx.gh.comments[0], "workspace.context") {
		t.Fatalf("want one dry-run comment with 'would apply', got %q", fx.gh.comments)
	}
	var ctxVal string
	dbfx.QueryRow(t, `SELECT context FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&ctxVal)
	if ctxVal != "old context" {
		t.Fatalf("dry run must not write workspace.context, got %q", ctxVal)
	}
}

func TestConfigSyncWebhookRejectsBadSignatureBeforeAnyRead(t *testing.T) {
	fx := newConfigSyncWebhookFixture(t, "owner")
	raw, _ := json.Marshal(fx.body("owner"))
	req := httptest.NewRequest("POST", "/api/webhooks/github", bytes.NewReader(raw))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	testutil.Call(t, testHandler.HandleGitHubWebhook, req).Want(http.StatusUnauthorized)
	if fx.factory != 0 || len(fx.gh.comments) != 0 {
		t.Fatalf("bad signature must trigger no GitHub read or comment, factory=%d comments=%d", fx.factory, len(fx.gh.comments))
	}
}

func TestConfigSyncWebhookTrustsAPIFieldsNotPayload(t *testing.T) {
	// The payload claims the owner merged; the API says someone else did.
	fx := newConfigSyncWebhookFixture(t, "intruder")
	postSignedGitHubWebhook(t, fx.secret, fx.body("owner"), "che3284-lie")

	if len(fx.gh.comments) != 1 || !strings.Contains(fx.gh.comments[0], "not owner merge") || strings.Contains(fx.gh.comments[0], "would apply") {
		t.Fatalf("want a 'not owner merge' receipt, got %q", fx.gh.comments)
	}
}

func TestConfigSyncWebhookIgnoresUnenrolledWorkspace(t *testing.T) {
	fx := newConfigSyncWebhookFixture(t, "owner")
	dbfx.Exec(t, `UPDATE workspace_config_sync SET settings = '' WHERE workspace_id = $1`, testWorkspaceID)
	postSignedGitHubWebhook(t, fx.secret, fx.body("owner"), "che3284-unenrolled")
	if fx.factory != 0 || len(fx.gh.comments) != 0 {
		t.Fatalf("unenrolled workspace must trigger nothing, factory=%d comments=%d", fx.factory, len(fx.gh.comments))
	}
}

func TestConfigSyncWebhookIgnoresOtherRepo(t *testing.T) {
	fx := newConfigSyncWebhookFixture(t, "owner")
	b := buildMergedPRWebhookBody("NONE-1", 7, "acme", "elsewhere", fx.instID)
	postSignedGitHubWebhook(t, fx.secret, b, "che3284-other-repo")
	if fx.factory != 0 {
		t.Fatalf("a PR in another repo must not trigger config sync, factory=%d", fx.factory)
	}
}

// The canonical form gate: configsync.Digest of a file's bytes equals the
// after-digest ownerCASWrite computes in the database for the same bytes.
func TestConfigSyncDigestMatchesOwnerCASWrite(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	var previous *string
	dbfx.QueryRow(t, `SELECT context FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previous)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET context = $1 WHERE id = $2`, previous, testWorkspaceID)
	})
	current := ""
	for name, file := range map[string]string{
		"plain":         "hello",
		"crlf":          "a\r\nb\r\n",
		"multibyte":     "héllo — 日本語 🙂",
		"trailing lf":   "line\n",
		"double lf":     "line\n\n",
		"leading space": "  x",
	} {
		dbfx.Exec(t, `UPDATE workspace SET context = $1 WHERE id = $2`, current, testWorkspaceID)
		res, reason, err := testHandler.ownerCASWrite(context.Background(), nil, ownerCASWorkspaceContext, testWorkspaceID, testWorkspaceID, file, testDigest(current))
		if reason != hermesExceptionRejectNone || err != nil {
			t.Fatalf("%s: write refused: reason=%v err=%v", name, reason, err)
		}
		if got := configsync.Digest([]byte(file)); got != res.AfterDigest {
			t.Errorf("%s: configsync.Digest %s != ownerCASWrite after_digest %s", name, got, res.AfterDigest)
		}
		current = file
	}
}

func TestConfigSyncLiveDigestIsScopedToTheWorkspace(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	otherWS := dbfx.Workspace(t, "che3284 other", "che3284-other")
	otherRuntime := dbfx.Runtime(t, "che3284-rt", testutil.Cols{"workspace_id": otherWS})
	otherAgent := dbfx.Agent(t, "che3284-other-agent", otherRuntime, testutil.Cols{"workspace_id": otherWS})
	ownAgent := createHandlerTestAgent(t, "che3284-own-agent", nil)
	dbfx.Exec(t, `UPDATE agent SET instructions = 'own' WHERE id = $1`, ownAgent)

	tg := configSyncTargets{h: testHandler, workspaceID: testWorkspaceID}
	ctx := context.Background()
	if d, found, err := tg.LiveDigest(ctx, "agent:"+ownAgent); err != nil || !found || d != testDigest("own") {
		t.Errorf("own agent: digest=%q found=%v err=%v", d, found, err)
	}
	if _, found, err := tg.LiveDigest(ctx, "agent:"+otherAgent); err != nil || found {
		t.Errorf("another workspace's agent must be 'not in workspace', found=%v err=%v", found, err)
	}
	if _, found, err := tg.LiveDigest(ctx, "agent:not-a-uuid"); err != nil || found {
		t.Errorf("malformed agent id: found=%v err=%v", found, err)
	}
}

// Every governed column writes an audit row of its own kind, so a column added
// without an auditKinds entry would answer 500 on its first write.
func TestOwnerCASColumnsHaveAuditKinds(t *testing.T) {
	for _, c := range []ownerCASColumn{ownerCASWorkspaceContext, ownerCASSquadInstructions, ownerCASAgentInstructions, ownerCASWorkspaceConfigSync} {
		if _, ok := auditKinds[c.kind]; !ok {
			t.Errorf("owner CAS column %q has no auditKinds entry", c.kind)
		}
	}
}

func TestConfigSyncOwnerCAS_WritesAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	cleanConfigSync(t)
	w := httptest.NewRecorder()
	testHandler.UpdateWorkspace(w, configSyncPatch(testUserID, configSyncSettingsJSON, testDigest("")))
	if w.Code != http.StatusOK {
		t.Fatalf("owner enroll: want 200, got %d: %s", w.Code, w.Body.String())
	}
	rows := auditRowsFor(t, "workspace.config_sync", testWorkspaceID)
	if len(rows) == 0 {
		t.Fatal("workspace.config_sync audit rows = 0; want 1")
	}
	row := rows[len(rows)-1]
	if row.ActorType != "member" || row.ActorID != testUserID || row.TargetType != "workspace" {
		t.Errorf("row = %+v; want a member row on a workspace target", row)
	}
	if encoded, _ := json.Marshal(row); strings.Contains(string(encoded), "acme/widget") {
		t.Errorf("settings text reached the audit row: %s", encoded)
	}
}
