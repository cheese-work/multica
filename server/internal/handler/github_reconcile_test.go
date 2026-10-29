package handler

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type fakeReconcileAPI struct {
	deliveries  []ghsnapshot.DeliveryAttempt
	pages       [][]ghsnapshot.DeliveryAttempt // when set, paged by cursor "pN" instead of deliveries
	redelivered []int64
	objects     ghsnapshot.PullObjects
	objectCalls int
}

func (api *fakeReconcileAPI) ListHookDeliveries(_ context.Context, cursor string) ([]ghsnapshot.DeliveryAttempt, string, error) {
	if api.pages == nil {
		return api.deliveries, "", nil
	}
	index := 0
	if cursor != "" {
		index, _ = strconv.Atoi(strings.TrimPrefix(cursor, "p"))
	}
	next := ""
	if index+1 < len(api.pages) {
		next = fmt.Sprintf("p%d", index+1)
	}
	return api.pages[index], next, nil
}

func (api *fakeReconcileAPI) RedeliverHook(_ context.Context, id int64) error {
	api.redelivered = append(api.redelivered, id)
	return nil
}

func (api *fakeReconcileAPI) ListPullObjects(context.Context, int64, string, string, int32) (ghsnapshot.PullObjects, error) {
	api.objectCalls++
	return api.objects, nil
}

func reconcilerForTest(fixture githubReviewFixture, api *fakeReconcileAPI) githubReconciler {
	return githubReconciler{handler: fixture.handler, api: api, now: time.Now}
}

func failedAttempt(id int64, guid string, age time.Duration) ghsnapshot.DeliveryAttempt {
	return ghsnapshot.DeliveryAttempt{ID: id, GUID: guid, Event: "pull_request_review", StatusCode: http.StatusBadGateway, DeliveredAt: time.Now().Add(-age)}
}

func TestGitHubReconcileT12aStoredDeliveryRetriesWithoutRedelivery(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	delivery := postIntakeForWorker(t, fixture, "-T12a")
	worker := intakeWorkerForTest(t, fixture)
	admit := worker.admit
	worker.admit = func(context.Context, *db.Queries, db.GithubWebhookRevision, githubIntakePayload) (githubIntakeDecision, error) {
		return githubIntakeDecision{}, context.DeadlineExceeded
	}
	if _, err := worker.runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertIntakeWorkerState(t, delivery, "deferred")
	api := &fakeReconcileAPI{deliveries: []ghsnapshot.DeliveryAttempt{failedAttempt(1, delivery, time.Hour)}}
	if n, err := reconcilerForTest(fixture, api).redeliver(context.Background()); err != nil || n != 0 || len(api.redelivered) != 0 {
		t.Fatalf("stored delivery was redelivered: n=%d calls=%v err=%v", n, api.redelivered, err)
	}
	dbfx.Exec(t, `UPDATE github_webhook_intake SET next_attempt_at=now()-interval '1 second' WHERE delivery_guid=$1`, delivery)
	worker.admit = admit
	if _, err := worker.runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertIntakeWorkerState(t, delivery, "dispatched")
	assertIntakeWorkerDispatch(t, fixture, 1)
}

func TestGitHubReconcileT12bRedeliversUnstoredFailures(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	delivery := fixture.issueID + "-T12b"
	old := time.Hour
	api := &fakeReconcileAPI{deliveries: []ghsnapshot.DeliveryAttempt{
		failedAttempt(11, delivery, old),
		failedAttempt(12, fixture.issueID+"-fresh", time.Minute),
		{ID: 13, GUID: fixture.issueID + "-ok", Event: "pull_request_review", StatusCode: 202, DeliveredAt: time.Now().Add(-old)},
		failedAttempt(14, fixture.issueID+"-ok", old),
		{ID: 15, GUID: fixture.issueID + "-ping", Event: "ping", StatusCode: 502, DeliveredAt: time.Now().Add(-old)},
	}}
	for id := int64(21); id < 26; id++ {
		api.deliveries = append(api.deliveries, failedAttempt(id, fixture.issueID+"-capped", old))
	}
	reconciler := reconcilerForTest(fixture, api)
	if n, err := reconciler.redeliver(context.Background()); err != nil || n != 1 || len(api.redelivered) != 1 || api.redelivered[0] != 11 {
		t.Fatalf("redelivered n=%d calls=%v err=%v, want only attempt 11", n, api.redelivered, err)
	}
	// GitHub's redelivery arrives signed through the normal path and dispatches once.
	if response := fixture.post("pull_request_review", fixture.secret, delivery); response.Code != http.StatusAccepted {
		t.Fatalf("redelivery = %d: %s", response.Code, response.Body.String())
	}
	if _, err := intakeWorkerForTest(t, fixture).runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertIntakeWorkerDispatch(t, fixture, 1)
	api.redelivered = nil
	if n, err := reconciler.redeliver(context.Background()); err != nil || n != 0 {
		t.Fatalf("stored redelivery asked again: n=%d calls=%v err=%v", n, api.redelivered, err)
	}
}

func unsignedObjects(fixture githubReviewFixture) ghsnapshot.PullObjects {
	old := time.Now().Add(-2 * time.Hour)
	signedSubmission := time.Date(2026, 10, 26, 8, 1, 0, 0, time.UTC) // the fixture's signed review 77
	recent := time.Now().Add(-time.Minute)
	return ghsnapshot.PullObjects{RepositoryID: 9001, HeadSHA: "review-head", Open: true, SameRepo: true, Objects: []ghsnapshot.PullObject{
		{Kind: "issue_comment", ID: 9001, Body: "missed comment", AuthorID: 42, AuthorLogin: "reviewer", AuthorType: "User", CreatedAt: &old, UpdatedAt: &old},
		{Kind: "issue_comment", ID: 9002, Body: "bot comment", AuthorID: 77, AuthorLogin: "ci[bot]", AuthorType: "Bot", CreatedAt: &old, UpdatedAt: &old},
		{Kind: "issue_comment", ID: 9003, Body: "just posted", AuthorID: 42, AuthorLogin: "reviewer", AuthorType: "User", CreatedAt: &recent, UpdatedAt: &recent},
		{Kind: "review", ID: 77, Body: "Please fix the error path.", AuthorID: 42, AuthorLogin: "reviewer", AuthorType: "User", ReviewState: "changes_requested", SubmittedAt: &signedSubmission},
		{Kind: "review", ID: 78, Body: "edited without a signed event", AuthorID: 42, AuthorLogin: "reviewer", AuthorType: "User", ReviewState: "commented", SubmittedAt: &old},
	}}
}

func TestGitHubReconcileT12cUnsignedIsNeverDispatched(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	postIntakeForWorker(t, fixture, "-T12c") // signed review 77 with the same body: covered
	api := &fakeReconcileAPI{objects: unsignedObjects(fixture)}
	reconciler := reconcilerForTest(fixture, api)
	if n, err := reconciler.recordUnsigned(context.Background()); err != nil || n != 2 {
		t.Fatalf("recorded %d unsigned revisions (err=%v), want comment 9001 and review 78", n, err)
	}
	if n, err := reconciler.recordUnsigned(context.Background()); err != nil || n != 0 {
		t.Fatalf("second pass recorded %d, want 0", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM github_webhook_revision WHERE workspace_id=$1 AND state='unsigned' AND source_delivery_guid IS NULL AND reason='no_signed_event' AND object_id IN (9001,78)`, testWorkspaceID); n != 2 {
		t.Fatalf("unsigned rows = %d, want 2", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM github_webhook_revision WHERE workspace_id=$1 AND object_id IN (9002,9003)`, testWorkspaceID); n != 0 {
		t.Fatalf("bot or in-grace objects recorded: %d", n)
	}
	worker := intakeWorkerForTest(t, fixture)
	for processed := true; processed; {
		processed, _ = worker.runOnce(context.Background())
	}
	assertIntakeWorkerDispatch(t, fixture, 1) // only the signed review
	if n := dbfx.Count(t, `SELECT count(*) FROM github_webhook_revision WHERE workspace_id=$1 AND state='unsigned' AND object_id IN (9001,78)`, testWorkspaceID); n != 2 {
		t.Fatalf("unsigned rows moved by the worker: %d remain", n)
	}
}

func TestGitHubReconcileT12cAlertsAfterOneHour(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	reconciler := reconcilerForTest(fixture, &fakeReconcileAPI{objects: unsignedObjects(fixture)})
	if _, err := reconciler.recordUnsigned(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, err := reconciler.alertStale(context.Background()); err != nil || n != 0 {
		t.Fatalf("fresh rows alerted: n=%d err=%v", n, err)
	}
	dbfx.Exec(t, `UPDATE github_webhook_revision SET created_at=now()-interval '61 minutes' WHERE workspace_id=$1 AND state='unsigned'`, testWorkspaceID)
	delivery := postIntakeForWorker(t, fixture, "-stale")
	dbfx.Exec(t, `UPDATE github_webhook_intake SET received_at=now()-interval '2 hours' WHERE delivery_guid=$1`, delivery)
	if n, err := reconciler.alertStale(context.Background()); err != nil || n != 4 {
		t.Fatalf("alerts = %d (err=%v), want 3 unsigned (comment 9001, reviews 77 and 78) + 1 deferred", n, err)
	}
}

func TestGitHubIntakeWorkerSettlesRevisionWithoutDelivery(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	delivery := postIntakeForWorker(t, fixture, "-no-guid")
	dbfx.Exec(t, `UPDATE github_webhook_revision SET source_delivery_guid=NULL WHERE source_delivery_guid=$1`, delivery)
	worker := intakeWorkerForTest(t, fixture)
	if _, err := worker.runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertIntakeWorkerState(t, delivery, "unsigned")
	if n := dbfx.Count(t, `SELECT count(*) FROM github_webhook_revision WHERE workspace_id=$1 AND state='unsigned' AND reason='no_signed_delivery'`, testWorkspaceID); n != 1 {
		t.Fatalf("revision without delivery not settled as unsigned: %d", n)
	}
	assertIntakeWorkerDispatch(t, fixture, 0)
}

// pagedLog spreads a delivery log over pages: filler holds recent successful
// deliveries of other GUIDs, and each extra attempt lands on its own page.
func pagedLog(pageCount int, extra map[int]ghsnapshot.DeliveryAttempt) [][]ghsnapshot.DeliveryAttempt {
	pages := make([][]ghsnapshot.DeliveryAttempt, pageCount)
	for index := range pages {
		pages[index] = []ghsnapshot.DeliveryAttempt{{ID: int64(1000 + index), GUID: fmt.Sprintf("filler-%d", index), Event: "pull_request_review", StatusCode: 202, DeliveredAt: time.Now().Add(-time.Hour)}}
		if attempt, ok := extra[index]; ok {
			pages[index] = append(pages[index], attempt)
		}
	}
	return pages
}

func TestGitHubReconcileCountsAttemptsAcrossPages(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	guid := fixture.issueID + "-paged"
	extra := map[int]ghsnapshot.DeliveryAttempt{0: failedAttempt(31, guid, time.Hour)}
	for index := 1; index <= 4; index++ {
		extra[index] = failedAttempt(int64(31+index), guid, time.Hour+time.Duration(index)*time.Minute)
	}
	api := &fakeReconcileAPI{pages: pagedLog(6, extra)}
	if n, err := reconcilerForTest(fixture, api).redeliver(context.Background()); err != nil || n != 0 || len(api.redelivered) != 0 {
		t.Fatalf("5 attempts across pages were redelivered: n=%d calls=%v err=%v", n, api.redelivered, err)
	}
}

func TestGitHubReconcileIncompleteLogRedeliversNothing(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	guid := fixture.issueID + "-deep"
	extra := map[int]ghsnapshot.DeliveryAttempt{0: failedAttempt(41, guid, time.Hour)}
	for index := 25; index <= 28; index++ {
		extra[index] = failedAttempt(int64(41+index), guid, time.Hour+time.Duration(index)*time.Second)
	}
	api := &fakeReconcileAPI{pages: pagedLog(30, extra)}
	if n, err := reconcilerForTest(fixture, api).redeliver(context.Background()); err == nil || n != 0 || len(api.redelivered) != 0 {
		t.Fatalf("incomplete log was acted on: n=%d calls=%v err=%v", n, api.redelivered, err)
	}
}

func TestGitHubReconcileSkipsDeliveriesOutsideTheWindow(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	api := &fakeReconcileAPI{deliveries: []ghsnapshot.DeliveryAttempt{
		failedAttempt(51, fixture.issueID+"-inside", 71*time.Hour),
		failedAttempt(52, fixture.issueID+"-expired", 73*time.Hour),
	}}
	if n, err := reconcilerForTest(fixture, api).redeliver(context.Background()); err != nil || n != 1 || len(api.redelivered) != 1 || api.redelivered[0] != 51 {
		t.Fatalf("redelivered n=%d calls=%v err=%v, want only attempt 51", n, api.redelivered, err)
	}
}

func TestGitHubReconcileRecordsCommentEditedBackToAnEarlierBody(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	now := time.Now()
	comment := func(body string, edited time.Duration) ghsnapshot.PullObjects {
		created, updated := now.Add(-4*time.Hour), now.Add(-edited)
		return ghsnapshot.PullObjects{RepositoryID: 9001, HeadSHA: "review-head", Open: true, SameRepo: true, Objects: []ghsnapshot.PullObject{
			{Kind: "issue_comment", ID: 9100, Body: body, AuthorID: 42, AuthorLogin: "reviewer", AuthorType: "User", CreatedAt: &created, UpdatedAt: &updated},
		}}
	}
	api := &fakeReconcileAPI{}
	reconciler := reconcilerForTest(fixture, api)
	for step, want := range []struct {
		body   string
		edited time.Duration
		n      int
	}{{"A", 3 * time.Hour, 1}, {"B", 2 * time.Hour, 1}, {"A", 90 * time.Minute, 1}, {"A", 90 * time.Minute, 0}, {"A", 80 * time.Minute, 1}, {"A", 80 * time.Minute, 0}} {
		api.objects = comment(want.body, want.edited)
		if n, err := reconciler.recordUnsigned(context.Background()); err != nil || n != want.n {
			t.Fatalf("step %d (%s): recorded %d (err=%v), want %d", step, want.body, n, err, want.n)
		}
	}
}

func TestGitHubReconcileRecordsReviewEditedBackWithIdenticalMetadata(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	submitted := time.Now().Add(-3 * time.Hour)
	api := &fakeReconcileAPI{}
	reconciler := reconcilerForTest(fixture, api)
	for step, want := range []struct {
		body string
		n    int
	}{{"A", 1}, {"B", 1}, {"A", 1}, {"A", 0}, {"B", 1}} {
		api.objects = ghsnapshot.PullObjects{RepositoryID: 9001, HeadSHA: "review-head", Open: true, SameRepo: true, Objects: []ghsnapshot.PullObject{
			{Kind: "review", ID: 9200, Body: want.body, AuthorID: 42, AuthorLogin: "reviewer", AuthorType: "User", ReviewState: "commented", SubmittedAt: &submitted},
		}}
		if n, err := reconciler.recordUnsigned(context.Background()); err != nil || n != want.n {
			t.Fatalf("step %d (%s): recorded %d (err=%v), want %d", step, want.body, n, err, want.n)
		}
	}
}
