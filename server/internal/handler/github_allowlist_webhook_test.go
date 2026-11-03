package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const allowlistWebhookSecret = "allowlist-webhook-secret"

// allowlistWebhookHandler is a copy of testHandler with the policy set to list
// and a started PR refresh manager whose fetches are counted.
func allowlistWebhookHandler(t *testing.T, list string) (*Handler, *atomic.Int32) {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	key, _ := generateTestRSAKeyPEM(t)
	t.Setenv("GITHUB_WEBHOOK_SECRET", allowlistWebhookSecret)
	t.Setenv("GITHUB_APP_ID", "123")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", string(key))
	client, err := ghsnapshot.NewClientFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	var fetches atomic.Int32
	h := *testHandler
	h.PRRefresh = ghsnapshot.NewManagerWithFetcher(client, h.Queries, testPool,
		func(context.Context, *ghsnapshot.Client, int64, string, string, int32) (*ghsnapshot.PRSnapshot, error) {
			fetches.Add(1)
			return nil, errors.New("stubbed snapshot fetch")
		}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	h.PRRefresh.Start(ctx)
	t.Cleanup(cancel)
	if err := h.ConfigureGitHubAllowlist(list); err != nil {
		t.Fatalf("configure allowlist %q: %v", list, err)
	}
	return &h, &fetches
}

func postAllowlistWebhook(t *testing.T, h *Handler, event, delivery string, body map[string]any) int {
	t.Helper()
	raw, _ := json.Marshal(body)
	mac := hmac.New(sha256.New, []byte(allowlistWebhookSecret))
	mac.Write(raw)
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/github", bytes.NewReader(raw))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	h.HandleGitHubWebhook(rec, req)
	return rec.Code
}

func intakeRows(t *testing.T, delivery string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM github_webhook_intake WHERE delivery_guid = $1`, delivery).Scan(&n); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM github_webhook_intake WHERE delivery_guid = $1`, delivery)
	})
	return n
}

func webhookRowCount(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestGitHubWebhookIntakeAllowlist(t *testing.T) {
	const base = int64(34140100)
	cases := []struct {
		name      string
		list      string
		bind      bool
		accountID *int64
		wantRow   bool
	}{
		{"unbound installation, policy on", "7001", false, nil, false},
		{"unbound installation, policy off", "", false, nil, false},
		{"bound, null account_id", "7001", true, nil, false},
		{"bound, unlisted account_id", "7001", true, ptr64(7999), false},
		{"bound, listed account_id", "7001", true, ptr64(7001), true},
		{"bound, policy off", "", true, nil, true},
	}
	for i, tc := range cases {
		for _, event := range []string{"issue_comment", "pull_request_review", "pull_request_review_comment"} {
			t.Run(tc.name+"/"+event, func(t *testing.T) {
				installationID := base + int64(i)
				if tc.bind {
					seedAllowlistRow(t, installationID, "acme", tc.accountID)
				}
				h, _ := allowlistWebhookHandler(t, tc.list)
				delivery := "che3414-intake-" + event + "-" + tc.name
				code := postAllowlistWebhook(t, h, event, delivery, map[string]any{
					"action": "created", "installation": map[string]any{"id": installationID},
				})
				if code != http.StatusAccepted {
					t.Fatalf("status = %d, want 202", code)
				}
				if got := intakeRows(t, delivery) == 1; got != tc.wantRow {
					t.Fatalf("intake row stored = %v, want %v", got, tc.wantRow)
				}
			})
		}
	}
}

func TestGitHubWebhookInstallationCreatedAllowlist(t *testing.T) {
	const installationID = int64(34140200)
	created := func(accountID int64) map[string]any {
		return map[string]any{"action": "created", "installation": map[string]any{
			"id": installationID, "account": map[string]any{"id": accountID, "login": "renamed", "type": "Organization"},
		}}
	}
	pending := func() int {
		return webhookRowCount(t, `SELECT count(*) FROM github_pending_installation WHERE installation_id = $1`, installationID)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM github_pending_installation WHERE installation_id = $1`, installationID)
	})

	t.Run("unbound installation stores no pending row, allowed account", func(t *testing.T) {
		h, _ := allowlistWebhookHandler(t, "7001")
		postAllowlistWebhook(t, h, "installation", "che3414-inst-1", created(7001))
		if pending() != 0 {
			t.Fatal("pending row stored for an unbound installation")
		}
	})
	t.Run("unbound installation stores no pending row, unlisted account", func(t *testing.T) {
		h, _ := allowlistWebhookHandler(t, "7001")
		postAllowlistWebhook(t, h, "installation", "che3414-inst-2", created(7999))
		if pending() != 0 {
			t.Fatal("pending row stored for an unlisted account")
		}
	})
	t.Run("unbound installation stores no pending row, policy off", func(t *testing.T) {
		h, _ := allowlistWebhookHandler(t, "")
		postAllowlistWebhook(t, h, "installation", "che3414-inst-3", created(7001))
		if pending() != 0 {
			t.Fatal("pending row stored for an unbound installation")
		}
	})
	login := func() string {
		var l string
		if err := testPool.QueryRow(context.Background(),
			`SELECT account_login FROM github_installation WHERE installation_id = $1`, installationID).Scan(&l); err != nil {
			t.Fatal(err)
		}
		return l
	}
	t.Run("bound but disallowed binding is not refreshed", func(t *testing.T) {
		seedAllowlistRow(t, installationID, "acme", ptr64(7999))
		h, _ := allowlistWebhookHandler(t, "7001")
		postAllowlistWebhook(t, h, "installation", "che3414-inst-4", created(7999))
		if got := login(); got != "acme" {
			t.Fatalf("login = %q, disallowed binding was refreshed", got)
		}
	})
	t.Run("bound and allowed binding is refreshed", func(t *testing.T) {
		seedAllowlistRow(t, installationID, "acme", ptr64(7001))
		h, _ := allowlistWebhookHandler(t, "7001")
		postAllowlistWebhook(t, h, "installation", "che3414-inst-5", created(7001))
		if got := login(); got != "renamed" {
			t.Fatalf("login = %q, want renamed", got)
		}
	})
	t.Run("deleted from any account still removes bindings", func(t *testing.T) {
		seedAllowlistRow(t, installationID, "acme", ptr64(7999))
		h, _ := allowlistWebhookHandler(t, "7001")
		body := created(7999)
		body["action"] = "deleted"
		postAllowlistWebhook(t, h, "installation", "che3414-inst-6", body)
		if n := webhookRowCount(t, `SELECT count(*) FROM github_installation WHERE installation_id = $1`, installationID); n != 0 {
			t.Fatalf("%d bindings survived deleted", n)
		}
	})
}

func TestGitHubWebhookPullRequestAllowlist(t *testing.T) {
	const base = int64(34140300)
	cases := []struct {
		name      string
		list      string
		bind      bool
		accountID *int64
		want      int
	}{
		{"unbound", "7001", false, nil, 0},
		{"null account_id", "7001", true, nil, 0},
		{"unlisted account_id", "7001", true, ptr64(7999), 0},
		{"listed account_id", "7001", true, ptr64(7001), 1},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			installationID := base + int64(i)
			if tc.bind {
				seedAllowlistRow(t, installationID, "acme", tc.accountID)
			}
			t.Cleanup(func() {
				_, _ = testPool.Exec(context.Background(), `DELETE FROM github_pull_request WHERE installation_id = $1`, installationID)
			})
			h, fetches := allowlistWebhookHandler(t, tc.list)
			body := buildMergedPRWebhookBody("NOPE-1", 3414, "acme", "widget", installationID)
			if code := postAllowlistWebhook(t, h, "pull_request", "che3414-pr-"+tc.name, body); code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202", code)
			}
			if n := webhookRowCount(t, `SELECT count(*) FROM github_pull_request WHERE installation_id = $1`, installationID); n != tc.want {
				t.Fatalf("mirrored PR rows = %d, want %d", n, tc.want)
			}
			if tc.want == 0 {
				time.Sleep(400 * time.Millisecond) // refresh jitter is up to 250ms
				if fetches.Load() != 0 {
					t.Fatal("PR refresh enqueued for a disallowed installation")
				}
			}
		})
	}
}

func TestGitHubWebhookCIEventAllowlist(t *testing.T) {
	const base = int64(34140400)
	cases := []struct {
		name      string
		bind      bool
		accountID *int64
		wantFetch bool
	}{
		{"unbound", false, nil, false},
		{"null account_id", true, nil, false},
		{"unlisted account_id", true, ptr64(7999), false},
		{"listed account_id", true, ptr64(7001), true},
	}
	for i, tc := range cases {
		for _, event := range []string{"check_suite", "check_run"} {
			t.Run(tc.name+"/"+event, func(t *testing.T) {
				installationID := base + int64(i)
				if tc.bind {
					seedAllowlistRow(t, installationID, "acme", tc.accountID)
				}
				// The refresh worker only fetches for a mirrored PR, so mirror one
				// (a row from before the policy, or a stale one, must stay inert).
				if _, err := testHandler.Queries.UpsertGitHubPullRequest(context.Background(), db.UpsertGitHubPullRequestParams{
					WorkspaceID: parseUUID(testWorkspaceID), InstallationID: installationID,
					RepoOwner: "acme", RepoName: "widget", PrNumber: 3414, Title: "t", State: "open", HtmlUrl: "https://example.test/pr/3414",
					PrCreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}, PrUpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
				}); err != nil {
					t.Fatalf("mirror PR: %v", err)
				}
				t.Cleanup(func() {
					_, _ = testPool.Exec(context.Background(), `DELETE FROM github_pull_request WHERE installation_id = $1`, installationID)
				})
				h, fetches := allowlistWebhookHandler(t, "7001")
				body := map[string]any{
					"installation": map[string]any{"id": installationID},
					"repository":   map[string]any{"name": "widget", "owner": map[string]any{"login": "acme"}},
				}
				body[event] = map[string]any{"pull_requests": []map[string]any{{"number": 3414}}}
				if code := postAllowlistWebhook(t, h, event, "che3414-ci-"+event+tc.name, body); code != http.StatusAccepted {
					t.Fatalf("status = %d, want 202", code)
				}
				deadline := time.Now().Add(time.Second)
				for tc.wantFetch && fetches.Load() == 0 && time.Now().Before(deadline) {
					time.Sleep(20 * time.Millisecond)
				}
				if !tc.wantFetch {
					time.Sleep(400 * time.Millisecond) // refresh jitter is up to 250ms
				}
				if got := fetches.Load() > 0; got != tc.wantFetch {
					t.Fatalf("refresh enqueued = %v, want %v", got, tc.wantFetch)
				}
			})
		}
	}
}

// Changing the list from L1 to L2 stops the next delivery for the same binding.
func TestGitHubWebhookAllowlistChangeStopsNextDelivery(t *testing.T) {
	const installationID = int64(34140500)
	seedAllowlistRow(t, installationID, "acme", ptr64(7001))
	body := map[string]any{"action": "created", "installation": map[string]any{"id": installationID}}

	l1, _ := allowlistWebhookHandler(t, "7001")
	postAllowlistWebhook(t, l1, "issue_comment", "che3414-l1", body)
	if intakeRows(t, "che3414-l1") != 1 {
		t.Fatal("L1 should store the delivery")
	}
	l2, _ := allowlistWebhookHandler(t, "8002")
	postAllowlistWebhook(t, l2, "issue_comment", "che3414-l2", body)
	if intakeRows(t, "che3414-l2") != 0 {
		t.Fatal("L2 should drop the delivery")
	}
}
