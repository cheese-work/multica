package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
)

// approvalFixture holds the unsigned issue comment 9001 (author 42, mapped to
// the test user) and a snapshot reader the test steers.
type approvalFixture struct {
	fixture  githubReviewFixture
	reader   *snapshotStub
	reviewID string
}

type snapshotStub struct {
	body  string
	err   error
	calls int
}

func (stub *snapshotStub) read(_ context.Context, _ int64, _, _ string, _ int32, kind string, id int64) (ghsnapshot.ApprovalSnapshot, error) {
	stub.calls++
	if stub.err != nil {
		return ghsnapshot.ApprovalSnapshot{}, stub.err
	}
	digest := sha256.Sum256([]byte(stub.body))
	snapshot := ghsnapshot.ApprovalSnapshot{ObjectKind: kind, ObjectID: id, AuthorID: 42, BodyHash: hex.EncodeToString(digest[:]), ReadAt: time.Now()}
	snapshot.Hash = snapshot.ContentHash()
	return snapshot, nil
}

func newApprovalFixture(t *testing.T) approvalFixture {
	t.Helper()
	fixture := newGitHubIntakeFixture(t)
	intakeWorkerForTest(t, fixture) // routing grant for the test user on the issue's project
	stub := &snapshotStub{body: "missed comment"}
	fixture.handler.readApprovalSnapshot = stub.read
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM github_snapshot_approval WHERE workspace_id=$1`, testWorkspaceID)
	})
	api := &fakeReconcileAPI{objects: unsignedObjects(fixture)}
	if _, err := reconcilerForTest(fixture, api).recordUnsigned(context.Background()); err != nil {
		t.Fatal(err)
	}
	var revisionID string
	dbfx.QueryRow(t, `SELECT id FROM github_webhook_revision WHERE workspace_id=$1 AND object_id=9001`, testWorkspaceID).Scan(&revisionID)
	return approvalFixture{fixture: fixture, reader: stub, reviewID: revisionID}
}

func (f approvalFixture) call(t *testing.T, method, suffix string, body any) *httptest.ResponseRecorder {
	t.Helper()
	request := withURLParam(newRequest(method, "/api/github/intake/revisions/"+f.reviewID+suffix, body), "id", f.reviewID)
	recorder := httptest.NewRecorder()
	if suffix == "/snapshot" {
		f.fixture.handler.GetGitHubRevisionSnapshot(recorder, request)
	} else {
		f.fixture.handler.ApproveGitHubRevisionSnapshot(recorder, request)
	}
	return recorder
}

func (f approvalFixture) snapshotHash(t *testing.T) string {
	t.Helper()
	recorder := f.call(t, http.MethodGet, "/snapshot", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("snapshot = %d: %s", recorder.Code, recorder.Body.String())
	}
	var out struct {
		SnapshotHash string `json:"snapshot_hash"`
	}
	decodeJSON(t, recorder.Body.String(), &out)
	return out.SnapshotHash
}

func (f approvalFixture) revisionState(t *testing.T) (state, reason string) {
	t.Helper()
	dbfx.QueryRow(t, `SELECT state, reason FROM github_webhook_revision WHERE id=$1`, f.reviewID).Scan(&state, &reason)
	return state, reason
}

func (f approvalFixture) assertNoEffects(t *testing.T) {
	t.Helper()
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id=$1`, f.fixture.issueID); n != 0 {
		t.Fatalf("%d comments projected", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND id<>$2`, f.fixture.issueID, f.fixture.source); n != 0 {
		t.Fatalf("%d runs queued", n)
	}
}

// T12c approval half: an exact snapshot approved by a member who passes can_route is projected once.
func TestGitHubApprovalT12cApprovedSnapshotProjectsOnce(t *testing.T) {
	f := newApprovalFixture(t)
	hash := f.snapshotHash(t)
	if recorder := f.call(t, http.MethodPost, "/approve", map[string]string{"snapshot_hash": hash}); recorder.Code != http.StatusOK {
		t.Fatalf("approve = %d: %s", recorder.Code, recorder.Body.String())
	}
	if state, reason := f.revisionState(t); state != "projected" || reason != "gate_g_closed" {
		t.Fatalf("revision = %s/%s, want projected/gate_g_closed", state, reason)
	}
	var approver, stored string
	dbfx.QueryRow(t, `SELECT approver_user_id::text, snapshot_hash FROM github_snapshot_approval WHERE revision_id=$1`, f.reviewID).Scan(&approver, &stored)
	if approver != testUserID || stored != hash {
		t.Fatalf("approval record = %s/%s, want %s/%s", approver, stored, testUserID, hash)
	}
	var content string
	dbfx.QueryRow(t, `SELECT content FROM comment WHERE issue_id=$1`, f.fixture.issueID).Scan(&content)
	if strings.Contains(content, "missed comment") || strings.Contains(content, "mention://") {
		t.Fatalf("GitHub text reached issue history: %s", content)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND id<>$2`, f.fixture.issueID, f.fixture.source); n != 0 {
		t.Fatalf("notify-only approval started %d runs", n)
	}
	if recorder := f.call(t, http.MethodPost, "/approve", map[string]string{"snapshot_hash": hash}); recorder.Code != http.StatusConflict {
		t.Fatalf("second approval = %d, want 409", recorder.Code)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id=$1`, f.fixture.issueID); n != 1 {
		t.Fatalf("projected %d comments, want exactly 1", n)
	}
}

func TestGitHubApprovalRejectsAHashThatIsNotTheCurrentSnapshot(t *testing.T) {
	f := newApprovalFixture(t)
	f.snapshotHash(t)
	if recorder := f.call(t, http.MethodPost, "/approve", map[string]string{"snapshot_hash": strings.Repeat("a", 64)}); recorder.Code != http.StatusConflict {
		t.Fatalf("stale hash = %d, want 409", recorder.Code)
	}
	if state, _ := f.revisionState(t); state != "unsigned" {
		t.Fatalf("revision = %s, want unsigned", state)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM github_snapshot_approval WHERE revision_id=$1`, f.reviewID); n != 0 {
		t.Fatalf("approval recorded for a stale hash")
	}
}

// The live object no longer matches the revision the reconciler recorded.
func TestGitHubApprovalRejectsAnObjectThatChangedSinceItWasRecorded(t *testing.T) {
	f := newApprovalFixture(t)
	f.reader.body = "edited since the reconciler read it"
	if recorder := f.call(t, http.MethodGet, "/snapshot", nil); recorder.Code != http.StatusConflict {
		t.Fatalf("changed object = %d, want 409", recorder.Code)
	}
}

func TestGitHubApprovalRequiresCanRoute(t *testing.T) {
	f := newApprovalFixture(t)
	hash := f.snapshotHash(t)
	dbfx.Exec(t, `DELETE FROM github_route_grant WHERE user_id=$1`, testUserID)
	if recorder := f.call(t, http.MethodPost, "/approve", map[string]string{"snapshot_hash": hash}); recorder.Code != http.StatusForbidden {
		t.Fatalf("approver without a grant = %d, want 403", recorder.Code)
	}
	if recorder := f.call(t, http.MethodGet, "/snapshot", nil); recorder.Code != http.StatusForbidden {
		t.Fatalf("snapshot without a grant = %d, want 403", recorder.Code)
	}
	if state, _ := f.revisionState(t); state != "unsigned" {
		t.Fatalf("revision = %s, want unsigned", state)
	}
	f.assertNoEffects(t)
}

func TestGitHubApprovalOnlyAppliesToAnUnsignedRevision(t *testing.T) {
	f := newApprovalFixture(t)
	hash := f.snapshotHash(t)
	dbfx.Exec(t, `UPDATE github_webhook_revision SET state='denied', reason='superseded' WHERE id=$1`, f.reviewID)
	if recorder := f.call(t, http.MethodPost, "/approve", map[string]string{"snapshot_hash": hash}); recorder.Code != http.StatusConflict {
		t.Fatalf("approve non-unsigned = %d, want 409", recorder.Code)
	}
}

// T12c: a body changed after approval returns the item to quarantined; the changed object needs its own approval.
func TestGitHubApprovalBodyChangedAfterApprovalQuarantines(t *testing.T) {
	f := newApprovalFixture(t)
	hash := f.snapshotHash(t)
	// The approve action reads once to compare, then the settle step reads again after the body changed.
	reads := 0
	f.fixture.handler.readApprovalSnapshot = func(ctx context.Context, installationID int64, owner, repo string, number int32, kind string, id int64) (ghsnapshot.ApprovalSnapshot, error) {
		reads++
		if reads > 1 {
			f.reader.body = "changed after approval"
		}
		return f.reader.read(ctx, installationID, owner, repo, number, kind, id)
	}
	if recorder := f.call(t, http.MethodPost, "/approve", map[string]string{"snapshot_hash": hash}); recorder.Code != http.StatusOK {
		t.Fatalf("approve = %d: %s", recorder.Code, recorder.Body.String())
	}
	if state, reason := f.revisionState(t); state != "quarantined" || reason != "approved_snapshot_changed" {
		t.Fatalf("revision = %s/%s, want quarantined/approved_snapshot_changed", state, reason)
	}
	f.assertNoEffects(t)
	// The reconciler records the changed body as a new unsigned revision; the old approval does not carry over.
	changed := unsignedObjects(f.fixture)
	for index := range changed.Objects {
		if changed.Objects[index].ID == 9001 {
			changed.Objects[index].Body = "changed after approval"
		}
	}
	if _, err := reconcilerForTest(f.fixture, &fakeReconcileAPI{objects: changed}).recordUnsigned(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM github_webhook_revision r WHERE r.workspace_id=$1 AND r.object_id=9001 AND r.state='unsigned' AND NOT EXISTS (SELECT 1 FROM github_snapshot_approval a WHERE a.revision_id=r.id)`, testWorkspaceID); n != 1 {
		t.Fatalf("changed object has %d unapproved unsigned revisions, want 1", n)
	}
	f.assertNoEffects(t)
}

// A lookup error at settle time leaves the approval pending; the reconciler retries it.
func TestGitHubApprovalLookupErrorIsRetried(t *testing.T) {
	f := newApprovalFixture(t)
	hash := f.snapshotHash(t)
	reads := 0
	f.fixture.handler.readApprovalSnapshot = func(ctx context.Context, installationID int64, owner, repo string, number int32, kind string, id int64) (ghsnapshot.ApprovalSnapshot, error) {
		reads++
		f.reader.err = nil
		if reads > 1 {
			f.reader.err = errors.New("github down")
		}
		return f.reader.read(ctx, installationID, owner, repo, number, kind, id)
	}
	if recorder := f.call(t, http.MethodPost, "/approve", map[string]string{"snapshot_hash": hash}); recorder.Code != http.StatusOK {
		t.Fatalf("approve = %d: %s", recorder.Code, recorder.Body.String())
	}
	if state, reason := f.revisionState(t); state != "deferred" || reason != "approved_snapshot" {
		t.Fatalf("revision = %s/%s, want deferred/approved_snapshot", state, reason)
	}
	f.assertNoEffects(t)
	f.fixture.handler.readApprovalSnapshot = f.reader.read
	f.reader.err = nil
	if n, err := reconcilerForTest(f.fixture, &fakeReconcileAPI{}).settleApproved(context.Background()); err != nil || n != 1 {
		t.Fatalf("settled %d (err=%v), want 1", n, err)
	}
	if state, _ := f.revisionState(t); state != "projected" {
		t.Fatalf("revision = %s, want projected", state)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id=$1`, f.fixture.issueID); n != 1 {
		t.Fatalf("projected %d comments, want 1", n)
	}
}

// Plan 5.2: a signed revision quarantined for a foreign editor is approved the same way.
func TestGitHubApprovalQuarantinedSignedRevisionIsApprovable(t *testing.T) {
	f := newApprovalFixture(t)
	dbfx.Exec(t, `UPDATE github_webhook_revision SET state='quarantined', reason='foreign_editor', source_delivery_guid='signed-delivery-1' WHERE id=$1`, f.reviewID)
	hash := f.snapshotHash(t)
	if recorder := f.call(t, http.MethodPost, "/approve", map[string]string{"snapshot_hash": hash}); recorder.Code != http.StatusOK {
		t.Fatalf("approve quarantined = %d: %s", recorder.Code, recorder.Body.String())
	}
	if state, reason := f.revisionState(t); state != "projected" || reason != "gate_g_closed" {
		t.Fatalf("revision = %s/%s, want projected/gate_g_closed", state, reason)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id=$1`, f.fixture.issueID); n != 1 {
		t.Fatalf("projected %d comments, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND id<>$2`, f.fixture.issueID, f.fixture.source); n != 0 {
		t.Fatalf("notify-only approval started %d runs", n)
	}
}

// A quarantined revision whose body changed cannot be approved: the live object is no longer the recorded one.
func TestGitHubApprovalQuarantinedRevisionWithChangedBodyIsRefused(t *testing.T) {
	f := newApprovalFixture(t)
	dbfx.Exec(t, `UPDATE github_webhook_revision SET state='quarantined', reason='body_changed', source_delivery_guid='signed-delivery-2' WHERE id=$1`, f.reviewID)
	f.reader.body = "edited by someone else"
	if recorder := f.call(t, http.MethodGet, "/snapshot", nil); recorder.Code != http.StatusConflict {
		t.Fatalf("snapshot of changed quarantined object = %d, want 409", recorder.Code)
	}
}

func TestGitHubApprovalRefusedWhileIntegrationIsDisabled(t *testing.T) {
	f := newApprovalFixture(t)
	hash := f.snapshotHash(t)
	setWorkspacePRWakeSetting(t, "github_enabled", false)
	if recorder := f.call(t, http.MethodPost, "/approve", map[string]string{"snapshot_hash": hash}); recorder.Code != http.StatusForbidden {
		t.Fatalf("approve while disabled = %d, want 403", recorder.Code)
	}
	if recorder := f.call(t, http.MethodGet, "/snapshot", nil); recorder.Code != http.StatusForbidden {
		t.Fatalf("snapshot while disabled = %d, want 403", recorder.Code)
	}
	if state, _ := f.revisionState(t); state != "unsigned" {
		t.Fatalf("revision = %s, want unsigned", state)
	}
	f.assertNoEffects(t)
}

// An approval left pending by an API outage must not project after the integration is switched off.
func TestGitHubApprovalSettlementRechecksTheIntegrationSwitch(t *testing.T) {
	f := newApprovalFixture(t)
	hash := f.snapshotHash(t)
	reads := 0
	f.fixture.handler.readApprovalSnapshot = func(ctx context.Context, installationID int64, owner, repo string, number int32, kind string, id int64) (ghsnapshot.ApprovalSnapshot, error) {
		reads++
		f.reader.err = nil
		if reads > 1 {
			f.reader.err = errors.New("github down")
		}
		return f.reader.read(ctx, installationID, owner, repo, number, kind, id)
	}
	if recorder := f.call(t, http.MethodPost, "/approve", map[string]string{"snapshot_hash": hash}); recorder.Code != http.StatusOK {
		t.Fatalf("approve = %d: %s", recorder.Code, recorder.Body.String())
	}
	if state, reason := f.revisionState(t); state != "deferred" || reason != "approved_snapshot" {
		t.Fatalf("revision = %s/%s, want deferred/approved_snapshot", state, reason)
	}
	setWorkspacePRWakeSetting(t, "github_enabled", false)
	f.fixture.handler.readApprovalSnapshot = f.reader.read
	f.reader.err = nil
	if _, err := reconcilerForTest(f.fixture, &fakeReconcileAPI{}).settleApproved(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state, reason := f.revisionState(t); state != "denied" || reason != "integration_disabled" {
		t.Fatalf("revision = %s/%s, want denied/integration_disabled", state, reason)
	}
	f.assertNoEffects(t)
}

// CHE-3700: the installation allowlist gates approval and settlement.
const approvalInstallationID int64 = 2019001

// setApprovalAllowlist binds the fixture installation to accountID and turns the
// policy on with `list`. The handler is shared, so the policy is restored.
func setApprovalAllowlist(t *testing.T, f approvalFixture, accountID int64, list string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE github_installation SET account_id=$1 WHERE installation_id=$2`, accountID, approvalInstallationID)
	t.Cleanup(func() { _ = f.fixture.handler.ConfigureGitHubAllowlist("") })
	if err := f.fixture.handler.ConfigureGitHubAllowlist(list); err != nil {
		t.Fatal(err)
	}
}

func TestGitHubApprovalRefusedForAnUnlistedInstallation(t *testing.T) {
	f := newApprovalFixture(t)
	hash := f.snapshotHash(t)
	f.reader.calls = 0
	setApprovalAllowlist(t, f, 7001, "7002")
	if recorder := f.call(t, http.MethodGet, "/snapshot", nil); recorder.Code != http.StatusForbidden {
		t.Fatalf("snapshot for unlisted installation = %d, want 403", recorder.Code)
	}
	if recorder := f.call(t, http.MethodPost, "/approve", map[string]string{"snapshot_hash": hash}); recorder.Code != http.StatusForbidden {
		t.Fatalf("approve for unlisted installation = %d, want 403", recorder.Code)
	}
	if f.reader.calls != 0 {
		t.Fatalf("%d GitHub reads for an unlisted installation, want 0", f.reader.calls)
	}
	if state, _ := f.revisionState(t); state != "unsigned" {
		t.Fatalf("revision = %s, want unsigned", state)
	}
	f.assertNoEffects(t)
}

// An approval stored before the installation was excluded must not read or project.
func TestGitHubApprovalSettlementDeniesAnUnlistedInstallation(t *testing.T) {
	f := newApprovalFixture(t)
	hash := f.snapshotHash(t)
	reads := 0
	f.fixture.handler.readApprovalSnapshot = func(ctx context.Context, installationID int64, owner, repo string, number int32, kind string, id int64) (ghsnapshot.ApprovalSnapshot, error) {
		reads++
		if reads > 1 {
			return ghsnapshot.ApprovalSnapshot{}, errors.New("github down")
		}
		return f.reader.read(ctx, installationID, owner, repo, number, kind, id)
	}
	if recorder := f.call(t, http.MethodPost, "/approve", map[string]string{"snapshot_hash": hash}); recorder.Code != http.StatusOK {
		t.Fatalf("approve = %d: %s", recorder.Code, recorder.Body.String())
	}
	if state, reason := f.revisionState(t); state != "deferred" || reason != "approved_snapshot" {
		t.Fatalf("revision = %s/%s, want deferred/approved_snapshot", state, reason)
	}
	f.fixture.handler.readApprovalSnapshot = f.reader.read
	f.reader.calls = 0
	setApprovalAllowlist(t, f, 7001, "7002")
	if _, err := reconcilerForTest(f.fixture, &fakeReconcileAPI{}).settleApproved(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.reader.calls != 0 {
		t.Fatalf("%d GitHub reads for an unlisted installation, want 0", f.reader.calls)
	}
	if state, reason := f.revisionState(t); state != "denied" || reason != "installation_not_allowed" {
		t.Fatalf("revision = %s/%s, want denied/installation_not_allowed", state, reason)
	}
	f.assertNoEffects(t)
}

func TestGitHubApprovalProjectsForAListedInstallation(t *testing.T) {
	f := newApprovalFixture(t)
	setApprovalAllowlist(t, f, 7001, "7001,7002")
	hash := f.snapshotHash(t)
	if recorder := f.call(t, http.MethodPost, "/approve", map[string]string{"snapshot_hash": hash}); recorder.Code != http.StatusOK {
		t.Fatalf("approve = %d: %s", recorder.Code, recorder.Body.String())
	}
	if state, reason := f.revisionState(t); state != "projected" || reason != "gate_g_closed" {
		t.Fatalf("revision = %s/%s, want projected/gate_g_closed", state, reason)
	}
}

// With the policy off, a binding removed after approval is a revoked target
// (the pre-existing outcome), never an allowlist denial.
func TestGitHubApprovalSettlementPolicyOffIgnoresAMissingBinding(t *testing.T) {
	f := newApprovalFixture(t)
	hash := f.snapshotHash(t)
	reads := 0
	f.fixture.handler.readApprovalSnapshot = func(ctx context.Context, installationID int64, owner, repo string, number int32, kind string, id int64) (ghsnapshot.ApprovalSnapshot, error) {
		reads++
		if reads > 1 {
			return ghsnapshot.ApprovalSnapshot{}, errors.New("github down")
		}
		return f.reader.read(ctx, installationID, owner, repo, number, kind, id)
	}
	if recorder := f.call(t, http.MethodPost, "/approve", map[string]string{"snapshot_hash": hash}); recorder.Code != http.StatusOK {
		t.Fatalf("approve = %d: %s", recorder.Code, recorder.Body.String())
	}
	f.fixture.handler.readApprovalSnapshot = f.reader.read
	dbfx.Exec(t, `DELETE FROM github_installation WHERE installation_id=$1`, approvalInstallationID)
	if _, err := reconcilerForTest(f.fixture, &fakeReconcileAPI{}).settleApproved(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state, reason := f.revisionState(t); state != "denied" || reason != "target_binding_revoked" {
		t.Fatalf("revision = %s/%s, want denied/target_binding_revoked", state, reason)
	}
}
