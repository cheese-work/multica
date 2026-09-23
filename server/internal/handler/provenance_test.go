package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		name string
		req  *http.Request
		want int
	}{
		{"empty allowlists", provenanceRequest(testWorkspaceID, testUserID, provenanceBody(testWorkspaceID, cutoff, nil, nil)), http.StatusBadRequest},
		{"workspace mismatch", provenanceRequest(testWorkspaceID, testUserID, provenanceBody("00000000-0000-0000-0000-000000000001", cutoff, []string{"HAN-1"}, nil)), http.StatusBadRequest},
		{"missing workspace", provenanceRequest(testWorkspaceID, testUserID, provenanceBody("", cutoff, []string{"HAN-1"}, nil)), http.StatusBadRequest},
		{"future cutoff", provenanceRequest(testWorkspaceID, testUserID, provenanceBody(testWorkspaceID, time.Now().Add(time.Hour), []string{"HAN-1"}, nil)), http.StatusBadRequest},
		{"bad cutoff", provenanceRequest(testWorkspaceID, testUserID, map[string]any{"workspace_id": testWorkspaceID, "issues": []string{"HAN-1"}, "cutoff": "yesterday"}), http.StatusBadRequest},
		{"unknown field", provenanceRequest(testWorkspaceID, testUserID, map[string]any{"workspace_id": testWorkspaceID, "issues": []string{"HAN-1"}, "cutoff": cutoff.Format(time.RFC3339), "chat_sessions": []string{"x"}}), http.StatusBadRequest},
		{"malformed body", provenanceRequest(testWorkspaceID, testUserID, "{not json"), http.StatusBadRequest},
		{"over source cap", provenanceRequest(testWorkspaceID, testUserID, provenanceBody(testWorkspaceID, cutoff, tooMany, nil)), http.StatusBadRequest},
		{"task token", testutil.WithHeaders(provenanceRequest(testWorkspaceID, testUserID, provenanceBody(testWorkspaceID, cutoff, []string{"HAN-1"}, nil)), "X-Actor-Source", "task_token"), http.StatusForbidden},
		{"cloud pat", testutil.WithHeaders(provenanceRequest(testWorkspaceID, testUserID, provenanceBody(testWorkspaceID, cutoff, []string{"HAN-1"}, nil)), "X-Actor-Source", "cloud_pat"), http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testutil.Call(t, testHandler.ExportProvenance, tc.req).Want(tc.want)
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

	plainUser := dbfx.User(t, "Prov Member", fmt.Sprintf("prov-member-%d@example.test", suffix))
	dbfx.Member(t, testWorkspaceID, plainUser, "member")
	testutil.Call(t, testHandler.ExportProvenance,
		provenanceRequest(testWorkspaceID, plainUser, provenanceBody(testWorkspaceID, cutoff, []string{"HAN-1"}, nil))).
		Want(http.StatusForbidden)

	adminUser := dbfx.User(t, "Prov Admin", fmt.Sprintf("prov-admin-%d@example.test", suffix))
	dbfx.Member(t, testWorkspaceID, adminUser, "admin")
	testutil.Call(t, testHandler.ExportProvenance,
		provenanceRequest(testWorkspaceID, adminUser, provenanceBody(testWorkspaceID, cutoff, []string{"HAN-999999"}, nil))).
		Want(http.StatusOK)

	otherWS := dbfx.Workspace(t, "Prov Other", fmt.Sprintf("prov-other-%d", suffix))
	testutil.Call(t, testHandler.ExportProvenance,
		provenanceRequest(otherWS, testUserID, provenanceBody(otherWS, cutoff, []string{"HAN-1"}, nil))).
		Want(http.StatusNotFound)
	if got := dbfx.Count(t, `SELECT count(*) FROM provenance_export_log WHERE workspace_id = $1`, otherWS); got != 0 {
		t.Fatalf("non-member export wrote %d audit rows", got)
	}
}

func TestExportProvenance_CutoffProvenanceRedactionAndAudit(t *testing.T) {
	requireProvenanceDB(t)
	provenanceCleanupLogs(t)
	cutoff := time.Now().Add(-time.Hour).Truncate(time.Second)
	suffix := time.Now().UnixNano()

	issueID := dbfx.Issue(t, "Provenance subject", testutil.Cols{"created_at": cutoff.Add(-2 * time.Hour)})
	var number int
	dbfx.QueryRow(t, `SELECT number FROM issue WHERE id = $1`, issueID).Scan(&number)

	before := dbfx.Comment(t, issueID, "before cutoff", testutil.Cols{"created_at": cutoff.Add(-time.Minute)})
	atCutoff := dbfx.Comment(t, issueID, "exactly at cutoff", testutil.Cols{"created_at": cutoff})
	after := dbfx.Comment(t, issueID, "after cutoff", testutil.Cols{"created_at": cutoff.Add(time.Minute)})
	secret := dbfx.Comment(t, issueID, "token "+provenanceFakeSecret(), testutil.Cols{"created_at": cutoff.Add(-30 * time.Second)})
	orphanAgent := dbfx.Comment(t, issueID, "agent without task", testutil.Cols{
		"created_at": cutoff.Add(-20 * time.Second), "author_type": "agent", "author_id": testUserID,
	})
	threadRoot := dbfx.Comment(t, issueID, "thread root", testutil.Cols{"created_at": cutoff.Add(-10 * time.Minute)})
	threadReply := dbfx.Comment(t, issueID, "thread reply", testutil.Cols{"created_at": cutoff.Add(-9 * time.Minute), "parent_id": threadRoot})
	threadLate := dbfx.Comment(t, issueID, "late reply", testutil.Cols{"created_at": cutoff.Add(2 * time.Minute), "parent_id": threadRoot})

	lateIssue := dbfx.Issue(t, "Created after cutoff", testutil.Cols{"created_at": cutoff.Add(time.Minute)})

	otherWS := dbfx.Workspace(t, "Prov Foreign", fmt.Sprintf("prov-foreign-%d", suffix))
	foreignIssue := dbfx.Issue(t, "Foreign", testutil.Cols{"workspace_id": otherWS, "created_at": cutoff.Add(-time.Hour)})
	foreignComment := dbfx.Comment(t, foreignIssue, "foreign", testutil.Cols{"workspace_id": otherWS, "created_at": cutoff.Add(-time.Hour)})

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
		lateIssue:                  service.ProvenanceOutOfCutoff,
		threadLate:                 service.ProvenanceOutOfCutoff,
		orphanAgent:                service.ProvenanceMissingProvenance,
		"issue:" + foreignIssue:    service.ProvenanceNotFoundOrDenied,
		"issue:OTHER-1":            service.ProvenanceNotFoundOrDenied,
		"thread:" + foreignComment: service.ProvenanceNotFoundOrDenied,
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
