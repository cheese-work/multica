package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func provenanceFakeSecret() string {
	return strings.Join([]string{"gh", "p_", strings.Repeat("Z9y8", 10)}, "")
}

type provenanceTestResponse struct {
	ExportID       string                        `json:"export_id"`
	RequestDigest  string                        `json:"request_digest"`
	ManifestDigest string                        `json:"manifest_digest"`
	Records        []service.ProvenanceRecord    `json:"records"`
	Exclusions     []service.ProvenanceExclusion `json:"exclusions"`
}

// provenanceUnchangedSince pins created_at and updated_at so a fixture row reads as
// unchanged since creation; fixtures otherwise default updated_at to now().
func provenanceUnchangedSince(at time.Time, extra ...testutil.Cols) testutil.Cols {
	cols := testutil.Cols{"created_at": at, "updated_at": at}
	for _, e := range extra {
		for k, v := range e {
			cols[k] = v
		}
	}
	return cols
}

func provenanceRequest(workspaceID, userID string, body any) *http.Request {
	return testutil.WithHeaders(testutil.JSONRequest(http.MethodPost, "/api/provenance/export", body),
		"X-User-ID", userID, "X-Workspace-ID", workspaceID)
}

func provenanceBody(workspaceID string, cutoff time.Time, issues, threads []string) map[string]any {
	return map[string]any{
		"workspace_id": workspaceID,
		"issues":       issues,
		"threads":      threads,
		"cutoff":       cutoff.UTC().Format(time.RFC3339),
	}
}

func provenanceCleanupLogs(t *testing.T) {
	dbfx.Cleanup(t, `DELETE FROM provenance_export_log WHERE workspace_id = $1`, testWorkspaceID)
}

func provenanceLogCount(t *testing.T) int {
	return dbfx.Count(t, `SELECT count(*) FROM provenance_export_log WHERE workspace_id = $1`, testWorkspaceID)
}

// captureProvenanceLogs routes the default slog logger into a buffer for the
// rest of the test.
func captureProvenanceLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

// requireRejectionLogged asserts the denied attempt left a trace naming the
// actor, the workspace and the reason, since no audit row is written for it.
func requireRejectionLogged(t *testing.T, logs *bytes.Buffer, actorID, workspaceID, reason string) {
	t.Helper()
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "provenance export: rejected") &&
			strings.Contains(line, "actor_id="+actorID) &&
			strings.Contains(line, "workspace_id="+workspaceID) &&
			strings.Contains(line, "reason="+reason) {
			logs.Reset()
			return
		}
	}
	t.Fatalf("no rejection log with actor=%s workspace=%s reason=%s:\n%s", actorID, workspaceID, reason, logs.String())
}

func requireProvenanceDB(t *testing.T) {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
}

func TestExportProvenance_RejectsBeforeDatabaseWork(t *testing.T) {
	requireProvenanceDB(t)
	provenanceCleanupLogs(t)
	cutoff := time.Now().Add(-time.Hour)
	tooMany := make([]string, service.ProvenanceMaxSources+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("HAN-%d", i+1)
	}
	cases := []struct {
		name   string
		req    *http.Request
		want   int
		reason string
	}{
		{"empty allowlists", provenanceRequest(testWorkspaceID, testUserID, provenanceBody(testWorkspaceID, cutoff, nil, nil)), http.StatusBadRequest, "no_sources"},
		{"workspace mismatch", provenanceRequest(testWorkspaceID, testUserID, provenanceBody("00000000-0000-0000-0000-000000000001", cutoff, []string{"HAN-1"}, nil)), http.StatusBadRequest, "workspace_mismatch"},
		{"missing workspace", provenanceRequest(testWorkspaceID, testUserID, provenanceBody("", cutoff, []string{"HAN-1"}, nil)), http.StatusBadRequest, "workspace_mismatch"},
		{"future cutoff", provenanceRequest(testWorkspaceID, testUserID, provenanceBody(testWorkspaceID, time.Now().Add(time.Hour), []string{"HAN-1"}, nil)), http.StatusBadRequest, "future_cutoff"},
		{"bad cutoff", provenanceRequest(testWorkspaceID, testUserID, map[string]any{"workspace_id": testWorkspaceID, "issues": []string{"HAN-1"}, "cutoff": "yesterday"}), http.StatusBadRequest, "invalid_cutoff"},
		{"unknown field", provenanceRequest(testWorkspaceID, testUserID, map[string]any{"workspace_id": testWorkspaceID, "issues": []string{"HAN-1"}, "cutoff": cutoff.Format(time.RFC3339), "chat_sessions": []string{"x"}}), http.StatusBadRequest, "invalid_body"},
		{"malformed body", provenanceRequest(testWorkspaceID, testUserID, "{not json"), http.StatusBadRequest, "invalid_body"},
		{"over source cap", provenanceRequest(testWorkspaceID, testUserID, provenanceBody(testWorkspaceID, cutoff, tooMany, nil)), http.StatusBadRequest, "too_many_sources"},
		{"task token", testutil.WithHeaders(provenanceRequest(testWorkspaceID, testUserID, provenanceBody(testWorkspaceID, cutoff, []string{"HAN-1"}, nil)), "X-Actor-Source", "task_token"), http.StatusForbidden, "machine_credential"},
		{"cloud pat", testutil.WithHeaders(provenanceRequest(testWorkspaceID, testUserID, provenanceBody(testWorkspaceID, cutoff, []string{"HAN-1"}, nil)), "X-Actor-Source", "cloud_pat"), http.StatusForbidden, "machine_credential"},
	}
	logs := captureProvenanceLogs(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testutil.Call(t, testHandler.ExportProvenance, tc.req).Want(tc.want)
			requireRejectionLogged(t, logs, testUserID, testWorkspaceID, tc.reason)
		})
	}
	if got := provenanceLogCount(t); got != 0 {
		t.Fatalf("audit rows = %d, want 0 after rejected requests", got)
	}
}

func TestExportProvenance_RequiresOwnerOrAdmin(t *testing.T) {
	requireProvenanceDB(t)
	provenanceCleanupLogs(t)
	cutoff := time.Now().Add(-time.Hour)
	suffix := time.Now().UnixNano()

	logs := captureProvenanceLogs(t)
	plainUser := dbfx.User(t, "Prov Member", fmt.Sprintf("prov-member-%d@example.test", suffix))
	dbfx.Member(t, testWorkspaceID, plainUser, "member")
	testutil.Call(t, testHandler.ExportProvenance,
		provenanceRequest(testWorkspaceID, plainUser, provenanceBody(testWorkspaceID, cutoff, []string{"HAN-1"}, nil))).
		Want(http.StatusForbidden)
	requireRejectionLogged(t, logs, plainUser, testWorkspaceID, "not_owner_or_admin")

	adminUser := dbfx.User(t, "Prov Admin", fmt.Sprintf("prov-admin-%d@example.test", suffix))
	dbfx.Member(t, testWorkspaceID, adminUser, "admin")
	testutil.Call(t, testHandler.ExportProvenance,
		provenanceRequest(testWorkspaceID, adminUser, provenanceBody(testWorkspaceID, cutoff, []string{"HAN-999999"}, nil))).
		Want(http.StatusOK)

	otherWS := dbfx.Workspace(t, "Prov Other", fmt.Sprintf("prov-other-%d", suffix))
	testutil.Call(t, testHandler.ExportProvenance,
		provenanceRequest(otherWS, testUserID, provenanceBody(otherWS, cutoff, []string{"HAN-1"}, nil))).
		Want(http.StatusNotFound)
	requireRejectionLogged(t, logs, testUserID, otherWS, "not_owner_or_admin")
	if got := dbfx.Count(t, `SELECT count(*) FROM provenance_export_log WHERE workspace_id = $1`, otherWS); got != 0 {
		t.Fatalf("non-member export wrote %d audit rows", got)
	}
}

func TestExportProvenance_CutoffProvenanceRedactionAndAudit(t *testing.T) {
	requireProvenanceDB(t)
	provenanceCleanupLogs(t)
	cutoff := time.Now().Add(-time.Hour).Truncate(time.Second)
	suffix := time.Now().UnixNano()

	issueID := dbfx.Issue(t, "Provenance subject", provenanceUnchangedSince(cutoff.Add(-2*time.Hour)))
	var number int
	dbfx.QueryRow(t, `SELECT number FROM issue WHERE id = $1`, issueID).Scan(&number)

	before := dbfx.Comment(t, issueID, "before cutoff", provenanceUnchangedSince(cutoff.Add(-time.Minute)))
	atCutoff := dbfx.Comment(t, issueID, "exactly at cutoff", provenanceUnchangedSince(cutoff))
	after := dbfx.Comment(t, issueID, "after cutoff", provenanceUnchangedSince(cutoff.Add(time.Minute)))
	secret := dbfx.Comment(t, issueID, "token "+provenanceFakeSecret(), provenanceUnchangedSince(cutoff.Add(-30*time.Second)))
	orphanAgent := dbfx.Comment(t, issueID, "agent without task", provenanceUnchangedSince(cutoff.Add(-20*time.Second),
		testutil.Cols{"author_type": "agent", "author_id": testUserID}))
	threadRoot := dbfx.Comment(t, issueID, "thread root", provenanceUnchangedSince(cutoff.Add(-10*time.Minute)))
	threadReply := dbfx.Comment(t, issueID, "thread reply", provenanceUnchangedSince(cutoff.Add(-9*time.Minute), testutil.Cols{"parent_id": threadRoot}))
	threadLate := dbfx.Comment(t, issueID, "late reply", provenanceUnchangedSince(cutoff.Add(2*time.Minute), testutil.Cols{"parent_id": threadRoot}))

	lateIssue := dbfx.Issue(t, "Created after cutoff", provenanceUnchangedSince(cutoff.Add(time.Minute)))

	otherWS := dbfx.Workspace(t, "Prov Foreign", fmt.Sprintf("prov-foreign-%d", suffix))
	foreignIssue := dbfx.Issue(t, "Foreign", provenanceUnchangedSince(cutoff.Add(-time.Hour), testutil.Cols{"workspace_id": otherWS}))
	foreignComment := dbfx.Comment(t, foreignIssue, "foreign", provenanceUnchangedSince(cutoff.Add(-time.Hour), testutil.Cols{"workspace_id": otherWS}))

	body := provenanceBody(testWorkspaceID, cutoff,
		[]string{fmt.Sprintf("HAN-%d", number), lateIssue, foreignIssue, "OTHER-1"},
		[]string{threadReply, threadLate, foreignComment, "not-a-uuid " + provenanceFakeSecret()})
	resp := testutil.Call(t, testHandler.ExportProvenance, provenanceRequest(testWorkspaceID, testUserID, body)).Want(http.StatusOK)
	if strings.Contains(resp.Text(), provenanceFakeSecret()) {
		t.Fatalf("response leaked the fake secret: %s", resp.Text())
	}
	var out provenanceTestResponse
	resp.JSON(&out)

	byID := map[string]service.ProvenanceRecord{}
	for _, r := range out.Records {
		byID[r.ID] = r
	}
	for _, id := range []string{issueID, before, atCutoff, secret, threadRoot, threadReply} {
		if _, ok := byID[id]; !ok {
			t.Errorf("record %s missing", id)
		}
	}
	for _, id := range []string{after, orphanAgent, threadLate, lateIssue, foreignIssue, foreignComment} {
		if _, ok := byID[id]; ok {
			t.Errorf("record %s must not be exported", id)
		}
	}
	if r := byID[secret]; !r.Redacted || !strings.Contains(r.Content, "[REDACTED") {
		t.Errorf("secret comment = %+v, want redacted", r)
	}
	if r := byID[before]; r.Redacted {
		t.Errorf("plain comment marked redacted: %+v", r)
	}

	reasons := map[string]service.ProvenanceExclusionReason{}
	for _, x := range out.Exclusions {
		key := x.ID
		if key == "" {
			key = x.Source
		}
		reasons[key] = x.Reason
	}
	wantReasons := map[string]service.ProvenanceExclusionReason{
		lateIssue:               service.ProvenanceOutOfCutoff,
		threadLate:              service.ProvenanceOutOfCutoff,
		orphanAgent:             service.ProvenanceMissingProvenance,
		"issue:" + foreignIssue: service.ProvenanceNotFoundOrDenied,
		service.ProvenanceSourceRef("issue", "OTHER-1", false):                             service.ProvenanceNotFoundOrDenied,
		"thread:" + foreignComment:                                                         service.ProvenanceNotFoundOrDenied,
		service.ProvenanceSourceRef("thread", "not-a-uuid "+provenanceFakeSecret(), false): service.ProvenanceMalformed,
	}
	for _, x := range out.Exclusions {
		// Comments after the cutoff are never selected by the issue source,
		// so they must not surface even as an id.
		if x.ID == after {
			t.Errorf("post-cutoff comment %s surfaced in exclusions", after)
		}
	}
	for key, want := range wantReasons {
		if got := reasons[key]; got != want {
			t.Errorf("exclusion[%s] = %q, want %q (all: %+v)", key, got, want, out.Exclusions)
		}
	}

	var (
		logDigest, logRequestDigest string
		manifest                    []byte
		included, excluded, sources int
	)
	dbfx.QueryRow(t, `SELECT manifest_digest, request_digest, manifest, included_count, excluded_count, source_count
		FROM provenance_export_log WHERE id = $1 AND workspace_id = $2`, out.ExportID, testWorkspaceID).
		Scan(&logDigest, &logRequestDigest, &manifest, &included, &excluded, &sources)
	if logDigest != out.ManifestDigest || logRequestDigest != out.RequestDigest {
		t.Fatalf("audit digests (%s, %s) != response (%s, %s)", logDigest, logRequestDigest, out.ManifestDigest, out.RequestDigest)
	}
	if included != len(out.Records) || excluded != len(out.Exclusions) || sources != 8 {
		t.Fatalf("audit counts included=%d excluded=%d sources=%d", included, excluded, sources)
	}
	for _, text := range []string{"before cutoff", "thread reply", "Provenance subject", "[REDACTED", "Z9y8"} {
		if strings.Contains(string(manifest), text) {
			t.Fatalf("audit manifest carries content %q: %s", text, manifest)
		}
	}
	var decoded service.ProvenanceManifest
	if err := json.Unmarshal(manifest, &decoded); err != nil || len(decoded.Records) != len(out.Records) {
		t.Fatalf("audit manifest decode err=%v records=%d", err, len(decoded.Records))
	}
}

func TestExportProvenance_AuditInsertFailureFailsClosed(t *testing.T) {
	requireProvenanceDB(t)
	provenanceCleanupLogs(t)
	cutoff := time.Now().Add(-time.Hour).Truncate(time.Second)
	issueID := dbfx.Issue(t, "Audit failure subject", testutil.Cols{"created_at": cutoff.Add(-time.Hour)})
	dbfx.Comment(t, issueID, "must not leave", testutil.Cols{"created_at": cutoff.Add(-time.Minute)})

	broken := *testHandler
	broken.Queries = db.New(failQueryDBTX{DBTX: testPool, failOn: "INSERT INTO provenance_export_log", err: errors.New("audit store down")})

	resp := testutil.Call(t, broken.ExportProvenance,
		provenanceRequest(testWorkspaceID, testUserID, provenanceBody(testWorkspaceID, cutoff, []string{issueID}, nil))).
		Want(http.StatusInternalServerError)
	if strings.Contains(resp.Text(), "must not leave") || strings.Contains(resp.Text(), "records") {
		t.Fatalf("failed export leaked data: %s", resp.Text())
	}
	if got := provenanceLogCount(t); got != 0 {
		t.Fatalf("audit rows = %d, want 0", got)
	}
}

func TestExportProvenance_ReadOnlyTransaction(t *testing.T) {
	requireProvenanceDB(t)
	tx, err := testHandler.TxStarter.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(context.Background(), "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY"); err != nil {
		t.Fatalf("set read only: %v", err)
	}
	if _, err := tx.Exec(context.Background(), `UPDATE issue SET title = title WHERE false`); err == nil {
		t.Fatal("write succeeded inside the export's read-only transaction mode")
	}
}

// provenanceExportOK runs one export as the test owner and returns the parsed
// response, the raw body and the persisted audit manifest.
func provenanceExportOK(t *testing.T, cutoff time.Time, issues, threads []string) (provenanceTestResponse, string, string) {
	t.Helper()
	resp := testutil.Call(t, testHandler.ExportProvenance,
		provenanceRequest(testWorkspaceID, testUserID, provenanceBody(testWorkspaceID, cutoff, issues, threads))).Want(http.StatusOK)
	var out provenanceTestResponse
	resp.JSON(&out)
	var manifest string
	dbfx.QueryRow(t, `SELECT manifest::text FROM provenance_export_log WHERE id = $1 AND workspace_id = $2`,
		out.ExportID, testWorkspaceID).Scan(&manifest)
	return out, resp.Text(), manifest
}

func provenanceOutcome(out provenanceTestResponse) (map[string]service.ProvenanceRecord, map[string]service.ProvenanceExclusionReason) {
	records := map[string]service.ProvenanceRecord{}
	for _, r := range out.Records {
		records[r.ID] = r
	}
	reasons := map[string]service.ProvenanceExclusionReason{}
	for _, x := range out.Exclusions {
		key := x.ID
		if key == "" {
			key = x.Source
		}
		reasons[key] = x.Reason
	}
	return records, reasons
}

func TestExportProvenance_RowsChangedAfterCutoff(t *testing.T) {
	requireProvenanceDB(t)
	provenanceCleanupLogs(t)
	cutoff := time.Now().Add(-time.Hour).Truncate(time.Second)
	created := cutoff.Add(-2 * time.Hour)

	issueID := dbfx.Issue(t, "Modified-after-cutoff subject", provenanceUnchangedSince(created))
	edited := dbfx.Comment(t, issueID, "edited after cutoff", provenanceUnchangedSince(created, testutil.Cols{"updated_at": cutoff.Add(time.Minute)}))
	deletedLater := dbfx.Comment(t, issueID, "deleted after cutoff", provenanceUnchangedSince(created, testutil.Cols{"deleted_at": cutoff.Add(time.Minute)}))
	deletedBefore := dbfx.Comment(t, issueID, "deleted before cutoff", provenanceUnchangedSince(created, testutil.Cols{"deleted_at": cutoff}))
	editedIssue := dbfx.Issue(t, "Renamed after cutoff", provenanceUnchangedSince(created, testutil.Cols{"updated_at": cutoff.Add(time.Minute)}))
	underEdited := dbfx.Comment(t, editedIssue, "comment on renamed issue", provenanceUnchangedSince(created))

	out, body, manifest := provenanceExportOK(t, cutoff, []string{issueID, editedIssue}, nil)
	records, reasons := provenanceOutcome(out)

	for id, want := range map[string]service.ProvenanceExclusionReason{
		edited:        service.ProvenanceModifiedAfterCutoff,
		deletedBefore: service.ProvenanceDeleted,
		editedIssue:   service.ProvenanceModifiedAfterCutoff,
	} {
		if got := reasons[id]; got != want {
			t.Errorf("exclusion[%s] = %q, want %q (all: %+v)", id, got, want, out.Exclusions)
		}
		if _, ok := records[id]; ok {
			t.Errorf("row %s exported despite %s", id, want)
		}
	}
	if r, ok := records[deletedLater]; !ok || r.Content != "deleted after cutoff" {
		t.Errorf("comment deleted after cutoff = %+v, present=%v; want exported unchanged", r, ok)
	}
	if _, ok := reasons[deletedLater]; ok {
		t.Errorf("comment deleted after cutoff was excluded: %+v", out.Exclusions)
	}
	if _, ok := records[underEdited]; !ok {
		t.Errorf("unchanged comment under an edited issue must still export on its own")
	}
	for _, text := range []string{"edited after cutoff", "Renamed after cutoff"} {
		if strings.Contains(body, text) || strings.Contains(manifest, text) {
			t.Fatalf("post-cutoff state %q leaked", text)
		}
	}
}

// A comment delete bumps its issue's revision through TouchIssueForCommentDelete
// without touching updated_at; the bumped revision did not exist at the cutoff
// and must not be certified by an exported record.
func TestExportProvenance_CommentDeleteAfterCutoffExcludesIssue(t *testing.T) {
	requireProvenanceDB(t)
	provenanceCleanupLogs(t)
	cutoff := time.Now().Add(-time.Hour).Truncate(time.Second)
	created := cutoff.Add(-2 * time.Hour)

	issueID := dbfx.Issue(t, "Comment deleted after cutoff", provenanceUnchangedSince(created))
	doomed := dbfx.Comment(t, issueID, "deleted after cutoff", provenanceUnchangedSince(created))
	survivor := dbfx.Comment(t, issueID, "survives", provenanceUnchangedSince(created))

	pre, _, _ := provenanceExportOK(t, cutoff, []string{issueID}, nil)
	preRecords, _ := provenanceOutcome(pre)
	preIssue, ok := preRecords[issueID]
	if !ok {
		t.Fatalf("issue must export before the delete (exclusions %+v)", pre.Exclusions)
	}

	if _, err := testHandler.deleteComment(context.Background(), parseUUID(doomed), parseUUID(testWorkspaceID)); err != nil {
		t.Fatalf("deleteComment: %v", err)
	}
	var (
		revision              int64
		updatedAt, activityAt time.Time
	)
	dbfx.QueryRow(t, `SELECT revision, updated_at, last_activity_at FROM issue WHERE id = $1`, issueID).
		Scan(&revision, &updatedAt, &activityAt)
	if revision != preIssue.Revision+1 || updatedAt.After(cutoff) || !activityAt.After(cutoff) {
		t.Fatalf("delete left revision=%d (was %d) updated_at=%s last_activity_at=%s; want revision bump and activity after cutoff %s with updated_at untouched",
			revision, preIssue.Revision, updatedAt, activityAt, cutoff)
	}

	out, _, _ := provenanceExportOK(t, cutoff, []string{issueID}, nil)
	records, reasons := provenanceOutcome(out)
	if r, ok := records[issueID]; ok {
		t.Fatalf("issue exported at post-cutoff revision %d: %+v", r.Revision, r)
	}
	if got := reasons[issueID]; got != service.ProvenanceModifiedAfterCutoff {
		t.Fatalf("exclusion[%s] = %q, want modified_after_cutoff (all: %+v)", issueID, got, out.Exclusions)
	}
	if _, ok := records[survivor]; !ok {
		t.Errorf("untouched comment under the excluded issue must still export (exclusions %+v)", out.Exclusions)
	}
}

func TestExportProvenance_IdentifierRefsNeverEchoed(t *testing.T) {
	requireProvenanceDB(t)
	provenanceCleanupLogs(t)
	cutoff := time.Now().Add(-time.Hour).Truncate(time.Second)

	// Shaped like an identifier (PREFIX-N) so it passes the loose well-formed
	// check, with a fake-secret prefix that is not this workspace's.
	secretRef := strings.Join([]string{provenanceFakeSecret(), "-", "1"}, "")
	var number int
	issueID := dbfx.Issue(t, "Identifier echo subject", provenanceUnchangedSince(cutoff.Add(-time.Hour)))
	dbfx.QueryRow(t, `SELECT number FROM issue WHERE id = $1`, issueID).Scan(&number)
	ownRef := fmt.Sprintf("HAN-%d", number)

	// The UUID ref comes first so it owns the deduplicated issue record.
	out, body, manifest := provenanceExportOK(t, cutoff, []string{secretRef, issueID, ownRef}, nil)
	for _, where := range []struct{ name, text string }{{"response", body}, {"audit manifest", manifest}} {
		if strings.Contains(where.text, provenanceFakeSecret()) {
			t.Fatalf("%s echoed the raw identifier ref: %s", where.name, where.text)
		}
		if strings.Contains(where.text, `"issue:`+ownRef+`"`) {
			t.Fatalf("%s echoed an identifier ref verbatim: %s", where.name, where.text)
		}
		if !strings.Contains(where.text, service.ProvenanceSourceRef("issue", secretRef, false)) {
			t.Fatalf("%s lacks the hashed ref: %s", where.name, where.text)
		}
	}
	records, reasons := provenanceOutcome(out)
	if got := reasons[service.ProvenanceSourceRef("issue", secretRef, false)]; got != service.ProvenanceNotFoundOrDenied {
		t.Fatalf("secret-shaped ref reason = %q (all: %+v)", got, out.Exclusions)
	}
	if r, ok := records[issueID]; !ok || r.Source != "issue:"+issueID {
		t.Fatalf("UUID ref should be echoed, got record %+v present=%v", r, ok)
	}
}

func TestExportProvenance_AgentRowsNeedWorkspaceTask(t *testing.T) {
	requireProvenanceDB(t)
	provenanceCleanupLogs(t)
	cutoff := time.Now().Add(-time.Hour).Truncate(time.Second)
	created := cutoff.Add(-2 * time.Hour)
	suffix := time.Now().UnixNano()

	agentID := dbfx.Agent(t, "prov agent", testRuntimeID)
	anchorIssue := dbfx.Issue(t, "Task anchor", provenanceUnchangedSince(created))
	ownTask := dbfx.Task(t, agentID, testutil.Cols{"issue_id": anchorIssue, "runtime_id": testRuntimeID})

	otherWS := dbfx.Workspace(t, "Prov Task Foreign", fmt.Sprintf("prov-task-foreign-%d", suffix))
	foreignRuntime := dbfx.Runtime(t, "prov foreign runtime", testutil.Cols{"workspace_id": otherWS})
	foreignAgent := dbfx.Agent(t, "prov foreign agent", foreignRuntime, testutil.Cols{"workspace_id": otherWS})
	foreignIssue := dbfx.Issue(t, "Foreign task anchor", provenanceUnchangedSince(created, testutil.Cols{"workspace_id": otherWS}))
	foreignTask := dbfx.Task(t, foreignAgent, testutil.Cols{"issue_id": foreignIssue, "runtime_id": foreignRuntime})
	missingTask := "00000000-0000-4000-8000-00000000c755"

	agentCols := func(taskID string) testutil.Cols {
		return provenanceUnchangedSince(created, testutil.Cols{"author_type": "agent", "author_id": agentID, "source_task_id": taskID})
	}
	subject := dbfx.Issue(t, "Agent provenance subject", provenanceUnchangedSince(created))
	viaOwn := dbfx.Comment(t, subject, "own task", agentCols(ownTask))
	viaForeign := dbfx.Comment(t, subject, "foreign task", agentCols(foreignTask))
	viaMissing := dbfx.Comment(t, subject, "missing task", agentCols(missingTask))

	agentIssue := func(title string, extra testutil.Cols) string {
		return dbfx.Issue(t, title, provenanceUnchangedSince(created, testutil.Cols{"creator_type": "agent", "creator_id": agentID}, extra))
	}
	issueOwn := agentIssue("agent issue own task", testutil.Cols{"origin_type": "agent_create", "origin_id": ownTask})
	issueForeign := agentIssue("agent issue foreign task", testutil.Cols{"origin_type": "agent_create", "origin_id": foreignTask})
	issueNoOrigin := agentIssue("agent issue without origin", testutil.Cols{})

	out, _, _ := provenanceExportOK(t, cutoff, []string{subject, issueOwn, issueForeign, issueNoOrigin}, nil)
	records, reasons := provenanceOutcome(out)

	for _, id := range []string{viaOwn, issueOwn} {
		if _, ok := records[id]; !ok {
			t.Errorf("row %s with a same-workspace task missing (exclusions %+v)", id, out.Exclusions)
		}
	}
	if r := records[issueOwn]; r.OriginType != "agent_create" || r.SourceTaskID != ownTask {
		t.Errorf("agent issue origin = (%q, %q), want (agent_create, %s)", r.OriginType, r.SourceTaskID, ownTask)
	}
	for _, id := range []string{viaForeign, viaMissing, issueForeign, issueNoOrigin} {
		if _, ok := records[id]; ok {
			t.Errorf("row %s exported without resolvable provenance", id)
		}
		if got := reasons[id]; got != service.ProvenanceMissingProvenance {
			t.Errorf("exclusion[%s] = %q, want missing_provenance", id, got)
		}
	}
}
