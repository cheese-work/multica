package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/featureflags"
)

// CHE-4280: the outcome labeler runs from the real entry points. A completed
// workflow_run webhook closes earlier failures; a member editing the owner
// issue marks them human_override. Both sit behind the ci_watcher flag.

const cdDeployWorkflow = ".github/workflows/cd-deploy.yml"

type outcomeRow struct {
	Label    string
	HasTTG   bool
	Reversed bool
}

func seedCIEvent(t *testing.T, signature string) string {
	t.Helper()
	var id string
	err := testPool.QueryRow(t.Context(), `INSERT INTO system1_observation
		(workspace_id, kind, subject_id, class, decision_source, inputs, created_at)
		VALUES ($1,'ci_event',gen_random_uuid(),'code','rule',
		        jsonb_build_object('repo','cheese-work/multica','workflow_path',$2::text,'branch','main','signature_hash',$3::text),
		        now() - interval '10 minutes') RETURNING id::text`,
		testWorkspaceID, cdDeployWorkflow, signature).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(t.Context(), `DELETE FROM system1_observation WHERE id = $1`, id)
		testPool.Exec(t.Context(), `DELETE FROM ci_signature_owner WHERE workspace_id = $1 AND signature_hash = $2`, testWorkspaceID, signature)
	})
	return id
}

func outcomeOf(t *testing.T, id string) outcomeRow {
	t.Helper()
	var r outcomeRow
	err := testPool.QueryRow(t.Context(), `SELECT coalesce(outcome_label,''), time_to_green_ms IS NOT NULL, reversed_at IS NOT NULL
		FROM system1_observation WHERE id = $1`, id).Scan(&r.Label, &r.HasTTG, &r.Reversed)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestWorkflowRunCompletedLabelsEarlierFailureWhenFlagOn(t *testing.T) {
	const installationID = int64(428000001)
	workflowFanoutFixture(t, installationID, "wf-outcome-b")
	withFeatureFlag(t, testHandler, featureflags.CIWatcher, true)
	id := seedCIEvent(t, "sig-wf-on")

	if code := postWorkflowEvent(t, "workflow_run", workflowRunEnvelope(t, installationID, "completed", "completed")); code != http.StatusAccepted {
		t.Fatalf("workflow_run status = %d", code)
	}
	if got := outcomeOf(t, id); got.Label != "self_healed" || !got.HasTTG {
		t.Fatalf("outcome = %+v, want self_healed with time_to_green_ms", got)
	}
}

func TestWorkflowRunCompletedLeavesFailureUntouchedWhenFlagOff(t *testing.T) {
	const installationID = int64(428000002)
	workflowFanoutFixture(t, installationID, "wf-outcome-off-b")
	withFeatureFlag(t, testHandler, featureflags.CIWatcher, false)
	id := seedCIEvent(t, "sig-wf-off")

	if code := postWorkflowEvent(t, "workflow_run", workflowRunEnvelope(t, installationID, "completed", "completed")); code != http.StatusAccepted {
		t.Fatalf("workflow_run status = %d", code)
	}
	if got := outcomeOf(t, id); got.Label != "" || got.HasTTG {
		t.Fatalf("outcome = %+v, want untouched with the flag off", got)
	}
}

func TestWorkflowRunInProgressDoesNotLabel(t *testing.T) {
	const installationID = int64(428000003)
	workflowFanoutFixture(t, installationID, "wf-outcome-prog-b")
	withFeatureFlag(t, testHandler, featureflags.CIWatcher, true)
	id := seedCIEvent(t, "sig-wf-prog")

	postWorkflowEvent(t, "workflow_run", workflowRunEnvelope(t, installationID, "in_progress", "in_progress"))
	if got := outcomeOf(t, id); got.Label != "" {
		t.Fatalf("outcome = %+v, want untouched before the run completes", got)
	}
}

// ownedFailure seeds a failure whose signature is owned by a fresh issue.
func ownedFailure(t *testing.T, signature string) (issueID, obsID string) {
	t.Helper()
	issueID = createTestIssue(t, "ci owner "+signature, "in_progress", "none")
	t.Cleanup(func() { deleteTestIssue(t, issueID) })
	obsID = seedCIEvent(t, signature)
	dbfx.Exec(t, `INSERT INTO ci_signature_owner (workspace_id, repo_owner, repo_name, signature_hash, issue_id)
		VALUES ($1,'cheese-work','multica',$2,$3)`, testWorkspaceID, signature, issueID)
	return issueID, obsID
}

func TestMemberIssueUpdateMarksHumanOverrideWhenFlagOn(t *testing.T) {
	withFeatureFlag(t, testHandler, featureflags.CIWatcher, true)
	issueID, obsID := ownedFailure(t, "sig-override-on")

	updateIssueField(t, issueID, map[string]any{"status": "in_review"})

	if got := outcomeOf(t, obsID); got.Label != "human_override" || !got.Reversed {
		t.Fatalf("outcome = %+v, want human_override with reversed_at", got)
	}
}

func TestMemberIssueUpdateLeavesFailureUntouchedWhenFlagOff(t *testing.T) {
	withFeatureFlag(t, testHandler, featureflags.CIWatcher, false)
	issueID, obsID := ownedFailure(t, "sig-override-off")

	updateIssueField(t, issueID, map[string]any{"status": "in_review"})

	if got := outcomeOf(t, obsID); got.Label != "" || got.Reversed {
		t.Fatalf("outcome = %+v, want untouched with the flag off", got)
	}
}

func TestMemberTitleEditIsNotAnOverride(t *testing.T) {
	withFeatureFlag(t, testHandler, featureflags.CIWatcher, true)
	issueID, obsID := ownedFailure(t, "sig-override-title")

	updateIssueField(t, issueID, map[string]any{"title": "renamed"})

	if got := outcomeOf(t, obsID); got.Label != "" {
		t.Fatalf("outcome = %+v, want a title edit to leave the failure open", got)
	}
}
