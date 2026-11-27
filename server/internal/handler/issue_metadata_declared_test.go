package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// declareMetadataKey registers key as declared for the test workspace, the way
// an enabled guard installation would. The installation id is a stand-in: rows
// of this table carry no foreign key.
func declareMetadataKey(t *testing.T, key string) {
	t.Helper()
	installationID := uuid.NewString()
	dbfx.Exec(t, `INSERT INTO plugin_guard_metadata_key (installation_id, workspace_id, metadata_key, enabled)
		VALUES ($1, $2, $3, TRUE)`, installationID, testWorkspaceID, key)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM plugin_guard_metadata_key WHERE installation_id = $1`, installationID)
	})
}

type metadataWrittenRow struct {
	Key     string `json:"key"`
	Value   any    `json:"value"`
	Deleted bool   `json:"deleted"`
	actor   string
}

func metadataWrittenRows(t *testing.T, issueID string) []metadataWrittenRow {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		`SELECT details, COALESCE(actor_id::text, '') FROM activity_log
		 WHERE issue_id = $1 AND action = 'metadata_written' ORDER BY created_at, id`, issueID)
	if err != nil {
		t.Fatalf("query metadata_written rows: %v", err)
	}
	defer rows.Close()
	var out []metadataWrittenRow
	for rows.Next() {
		var details []byte
		var row metadataWrittenRow
		if err := rows.Scan(&details, &row.actor); err != nil {
			t.Fatalf("scan metadata_written row: %v", err)
		}
		if err := json.Unmarshal(details, &row); err != nil {
			t.Fatalf("decode metadata_written details %s: %v", details, err)
		}
		out = append(out, row)
	}
	return out
}

func TestDeclaredMetadataKeyIdenticalValueBumpsRevisionAndRecordsWriter(t *testing.T) {
	declareMetadataKey(t, "declared_identical")
	issueID := dbfx.Issue(t, "declared identical value", testutil.Cols{
		"metadata": testutil.Raw(`'{"declared_identical":"same"}'::jsonb`),
		"revision": 3,
	})

	var body metadataMutationResponse
	testutil.Call(t, testHandler.SetIssueMetadataKey,
		mutationRequest(http.MethodPut, issueID, "declared_identical", map[string]any{"value": "same"})).
		Want(http.StatusOK).JSON(&body)

	if body.IssueRevision != 4 || body.Metadata["declared_identical"] != "same" {
		t.Fatalf("response = %+v, want revision 4 with the value kept", body)
	}
	if after := readMetadataRowState(t, issueID); after.revision != 4 {
		t.Fatalf("stored revision = %d, want 4", after.revision)
	}
	rows := metadataWrittenRows(t, issueID)
	if len(rows) != 1 || rows[0].Key != "declared_identical" || rows[0].Value != "same" || rows[0].Deleted || rows[0].actor != testUserID {
		t.Fatalf("metadata_written rows = %+v, want one row for key, value and the calling member", rows)
	}

	// A second identical declaration is a new writer record, not a replay.
	testutil.Call(t, testHandler.SetIssueMetadataKey,
		mutationRequest(http.MethodPut, issueID, "declared_identical", map[string]any{"value": "same"})).
		Want(http.StatusOK).JSON(&body)
	if body.IssueRevision != 5 || len(metadataWrittenRows(t, issueID)) != 2 {
		t.Fatalf("second declaration: revision=%d rows=%d, want 5 and 2", body.IssueRevision, len(metadataWrittenRows(t, issueID)))
	}
}

func TestDeclaredMetadataKeyChangedValueRecordsWriter(t *testing.T) {
	declareMetadataKey(t, "declared_changed")
	issueID := dbfx.Issue(t, "declared changed value")
	before := readMetadataRowState(t, issueID)

	var body metadataMutationResponse
	testutil.Call(t, testHandler.SetIssueMetadataKey,
		mutationRequest(http.MethodPut, issueID, "declared_changed", map[string]any{"value": "first"})).
		Want(http.StatusOK).JSON(&body)

	if body.IssueRevision != before.revision+1 || body.Metadata["declared_changed"] != "first" {
		t.Fatalf("response = %+v, want revision %d", body, before.revision+1)
	}
	if rows := metadataWrittenRows(t, issueID); len(rows) != 1 || rows[0].Value != "first" {
		t.Fatalf("metadata_written rows = %+v, want one row with the value", rows)
	}
}

func TestDeclaredMetadataKeyDeleteRecordsWriter(t *testing.T) {
	declareMetadataKey(t, "declared_deleted")
	issueID := dbfx.Issue(t, "declared delete", testutil.Cols{
		"metadata": testutil.Raw(`'{"declared_deleted":"x"}'::jsonb`),
		"revision": 1,
	})

	var body metadataMutationResponse
	testutil.Call(t, testHandler.DeleteIssueMetadataKey,
		mutationRequest(http.MethodDelete, issueID, "declared_deleted", nil)).
		Want(http.StatusOK).JSON(&body)

	if body.IssueRevision != 2 {
		t.Fatalf("revision = %d, want 2", body.IssueRevision)
	}
	if _, present := body.Metadata["declared_deleted"]; present {
		t.Fatalf("key survived the delete: %+v", body.Metadata)
	}
	if rows := metadataWrittenRows(t, issueID); len(rows) != 1 || !rows[0].Deleted || rows[0].Key != "declared_deleted" {
		t.Fatalf("metadata_written rows = %+v, want one delete row", rows)
	}

	// Deleting a key that is already absent changes nothing, declared or not.
	before := readMetadataRowState(t, issueID)
	testutil.Call(t, testHandler.DeleteIssueMetadataKey,
		mutationRequest(http.MethodDelete, issueID, "declared_deleted", nil)).Want(http.StatusOK)
	if after := readMetadataRowState(t, issueID); after != before || len(metadataWrittenRows(t, issueID)) != 1 {
		t.Fatalf("delete of an absent declared key changed state or wrote a row")
	}
}

// A write that fails after the issue update must leave neither the new
// revision nor a writer row: the two commit or roll back together.
func TestDeclaredMetadataKeyRolledBackWriteLeavesNeitherRevisionNorRow(t *testing.T) {
	declareMetadataKey(t, "declared_rollback")
	issueID := dbfx.Issue(t, "declared rollback", testutil.Cols{
		"metadata": testutil.Raw(`'{"declared_rollback":"same"}'::jsonb`),
		"revision": 5,
	})
	before := readMetadataRowState(t, issueID)

	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE OR REPLACE FUNCTION che10257_fail_metadata_written() RETURNS trigger AS $$
		 BEGIN
		   IF NEW.action = 'metadata_written' AND NEW.issue_id = '` + issueID + `'::uuid THEN
		     RAISE EXCEPTION 'forced metadata_written failure';
		   END IF;
		   RETURN NEW;
		 END $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER che10257_fail_metadata_written BEFORE INSERT ON activity_log
		 FOR EACH ROW EXECUTE FUNCTION che10257_fail_metadata_written()`,
	} {
		if _, err := testPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("install failure trigger: %v", err)
		}
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DROP TRIGGER IF EXISTS che10257_fail_metadata_written ON activity_log`)
		testPool.Exec(ctx, `DROP FUNCTION IF EXISTS che10257_fail_metadata_written()`)
	})

	testutil.Call(t, testHandler.SetIssueMetadataKey,
		mutationRequest(http.MethodPut, issueID, "declared_rollback", map[string]any{"value": "same"})).
		Want(http.StatusInternalServerError)

	if after := readMetadataRowState(t, issueID); after != before {
		t.Fatalf("rolled-back write changed the issue:\n before=%+v\n after=%+v", before, after)
	}
	if rows := metadataWrittenRows(t, issueID); len(rows) != 0 {
		t.Fatalf("rolled-back write left %d metadata_written rows", len(rows))
	}
}

// Undeclared keys keep today's behavior byte for byte: an identical value is a
// no-op and records no writer.
func TestUndeclaredMetadataKeyIdenticalValueStaysNoop(t *testing.T) {
	declareMetadataKey(t, "some_other_key")
	issueID := dbfx.Issue(t, "undeclared identical value", testutil.Cols{
		"metadata": testutil.Raw(`'{"undeclared_key":"same"}'::jsonb`),
		"revision": 7,
	})
	before := readMetadataRowState(t, issueID)

	var body metadataMutationResponse
	testutil.Call(t, testHandler.SetIssueMetadataKey,
		mutationRequest(http.MethodPut, issueID, "undeclared_key", map[string]any{"value": "same"})).
		Want(http.StatusOK).JSON(&body)

	if body.IssueRevision != 7 || body.Metadata["undeclared_key"] != "same" {
		t.Fatalf("response = %+v, want the unchanged revision 7 snapshot", body)
	}
	if after := readMetadataRowState(t, issueID); after != before {
		t.Fatalf("undeclared no-op changed the row:\n before=%+v\n after=%+v", before, after)
	}
	if rows := metadataWrittenRows(t, issueID); len(rows) != 0 {
		t.Fatalf("undeclared write recorded %d metadata_written rows", len(rows))
	}
}

// A disabled declaration declares nothing.
func TestDisabledDeclarationDoesNotDeclareTheKey(t *testing.T) {
	declareMetadataKey(t, "declared_disabled")
	dbfx.Exec(t, `UPDATE plugin_guard_metadata_key SET enabled = FALSE
		WHERE workspace_id = $1 AND metadata_key = 'declared_disabled'`, testWorkspaceID)
	issueID := dbfx.Issue(t, "disabled declaration", testutil.Cols{
		"metadata": testutil.Raw(`'{"declared_disabled":"same"}'::jsonb`),
		"revision": 2,
	})

	testutil.Call(t, testHandler.SetIssueMetadataKey,
		mutationRequest(http.MethodPut, issueID, "declared_disabled", map[string]any{"value": "same"})).
		Want(http.StatusOK)

	if after := readMetadataRowState(t, issueID); after.revision != 2 {
		t.Fatalf("revision = %d, want the no-op revision 2", after.revision)
	}
	if rows := metadataWrittenRows(t, issueID); len(rows) != 0 {
		t.Fatalf("disabled declaration recorded %d rows", len(rows))
	}
}
