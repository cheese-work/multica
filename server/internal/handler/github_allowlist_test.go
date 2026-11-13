package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestParseGitHubAllowedAccountIDs(t *testing.T) {
	t.Run("unset and blank are off", func(t *testing.T) {
		for _, raw := range []string{"", "  "} {
			a, err := parseGitHubAllowedAccountIDs(raw)
			if err != nil || a.ids != nil {
				t.Fatalf("parse(%q) = %+v, %v; want policy off", raw, a, err)
			}
		}
	})
	t.Run("trims spaces", func(t *testing.T) {
		a, err := parseGitHubAllowedAccountIDs(" 11, 22 ,33")
		if err != nil || len(a.ids) != 3 {
			t.Fatalf("parse = %+v, %v; want 3 ids", a, err)
		}
		for _, id := range []int64{11, 22, 33} {
			if _, ok := a.ids[id]; !ok {
				t.Fatalf("id %d missing from %+v", id, a.ids)
			}
		}
	})
	t.Run("malformed entry is an error", func(t *testing.T) {
		for _, raw := range []string{"11,octocat", "abc", "1.5", "0", "-3", "11;22", ",", " , ", "11,", ",11", "11,,22"} {
			if _, err := parseGitHubAllowedAccountIDs(raw); err == nil ||
				!strings.Contains(err.Error(), "GITHUB_ALLOWED_ACCOUNT_IDS") {
				t.Fatalf("parse(%q) err = %v; want GITHUB_ALLOWED_ACCOUNT_IDS error", raw, err)
			}
		}
	})
}

func allowlistHandler(t *testing.T, raw string) *Handler {
	t.Helper()
	h := &Handler{Queries: testHandler.Queries}
	if err := h.ConfigureGitHubAllowlist(raw); err != nil {
		t.Fatalf("configure allowlist %q: %v", raw, err)
	}
	return h
}

func accountRow(id *int64, login string) db.GithubInstallation {
	row := db.GithubInstallation{AccountLogin: login}
	if id != nil {
		row.AccountID.Valid, row.AccountID.Int64 = true, *id
	}
	return row
}

func ptr64(v int64) *int64 { return &v }

func TestGitHubInstallationAllowedPolicy(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	tests := []struct {
		name string
		list string
		row  db.GithubInstallation
		want bool
	}{
		{"list unset allows null id", "", accountRow(nil, "acme"), true},
		{"list unset allows any id", "", accountRow(ptr64(5), "acme"), true},
		{"listed id allowed", "5,6", accountRow(ptr64(5), "acme"), true},
		{"unlisted id denied", "6", accountRow(ptr64(5), "acme"), false},
		{"list set, null id denied", "5", accountRow(nil, "acme"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := allowlistHandler(t, tc.list).githubInstallationAllowed(tc.row); got != tc.want {
				t.Fatalf("allowed = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("list change flips the verdict for the same row", func(t *testing.T) {
		row := accountRow(ptr64(5), "acme")
		h := allowlistHandler(t, "5")
		if !h.githubInstallationAllowed(row) {
			t.Fatal("L1 should allow")
		}
		if err := h.ConfigureGitHubAllowlist("6"); err != nil {
			t.Fatal(err)
		}
		if h.githubInstallationAllowed(row) {
			t.Fatal("L2 should deny")
		}
	})
}

func seedAllowlistRow(t *testing.T, installationID int64, login string, accountID *int64) db.GithubInstallation {
	t.Helper()
	ctx := context.Background()
	params := db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		InstallationID: installationID,
		AccountLogin:   login,
		AccountType:    "Organization",
	}
	if accountID != nil {
		params.AccountID.Valid, params.AccountID.Int64 = true, *accountID
	}
	row, err := testHandler.Queries.CreateGitHubInstallation(ctx, params)
	if err != nil {
		t.Fatalf("seed installation: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM github_installation WHERE installation_id = $1`, installationID)
	})
	return row
}

// Two bindings share a login; only the one whose numeric id is listed passes.
// Login is display data: any /users/ lookup fails the test.
func TestAllowedGitHubInstallationsRenamedReusedLogin(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID = int64(34180001)
	first := seedAllowlistRow(t, installationID, "acme", ptr64(7001))
	// A second workspace binding of the same installation, reused login, new id.
	ws2 := parseUUID(seedForeignWorkspace(t))
	second, err := testHandler.Queries.CreateGitHubInstallation(context.Background(), db.CreateGitHubInstallationParams{
		WorkspaceID: ws2, InstallationID: installationID, AccountLogin: "acme", AccountType: "Organization",
		AccountID: pgtype.Int8{Int64: 7002, Valid: true},
	})
	if err != nil {
		t.Fatalf("seed second binding: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/users/") {
			t.Errorf("login lookup forbidden: %s", r.URL.Path)
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	old := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = old })

	h := allowlistHandler(t, "7001")
	rows, err := h.allowedGitHubInstallations(context.Background(), installationID)
	if err != nil {
		t.Fatalf("allowedGitHubInstallations: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != first.ID {
		t.Fatalf("allowed rows = %+v, want only first (%v), not second (%v)", rows, first.ID, second.ID)
	}
	h.BackfillGitHubAccountIDs(context.Background())
}

func TestGitHubAccountIDCapture(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()

	t.Run("bind with code writes the verified account id", func(t *testing.T) {
		const installationID = int64(34180002)
		t.Cleanup(func() {
			_, _ = testPool.Exec(ctx, `DELETE FROM github_installation WHERE installation_id = $1`, installationID)
		})
		pemBytes, _ := generateTestRSAKeyPEM(t)
		t.Setenv("GITHUB_WEBHOOK_SECRET", "capture-secret")
		t.Setenv("GITHUB_APP_CLIENT_ID", "capture-client")
		t.Setenv("GITHUB_APP_CLIENT_SECRET", "capture-client-secret")
		t.Setenv("GITHUB_APP_ID", "268")
		t.Setenv("GITHUB_APP_PRIVATE_KEY", string(pemBytes))
		t.Setenv("FRONTEND_ORIGIN", "https://app.example.test")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/login/oauth/access_token":
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok"})
			case fmt.Sprintf("/app/installations/%d", installationID):
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": installationID, "account": map[string]any{"id": 9001, "login": "owner", "type": "User"},
				})
			case "/user":
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 9001, "login": "owner"})
			case "/applications/capture-client/token":
				w.WriteHeader(http.StatusNoContent)
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(srv.Close)
		oldAPI, oldOAuth := githubAPIBase, githubOAuthBase
		githubAPIBase, githubOAuthBase = srv.URL, srv.URL
		t.Cleanup(func() { githubAPIBase, githubOAuthBase = oldAPI, oldOAuth })

		state, err := signState(testWorkspaceID)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf(
			"/api/github/setup?installation_id=%d&code=c&state=%s", installationID, url.QueryEscape(state)), nil)
		rec := httptest.NewRecorder()
		testHandler.GitHubSetupCallback(rec, req)
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "github_connected=1") {
			t.Fatalf("redirect = %q, want success", loc)
		}
		rows, _ := testHandler.Queries.ListGitHubInstallationsByInstallationID(ctx, installationID)
		if len(rows) != 1 || !rows[0].AccountID.Valid || rows[0].AccountID.Int64 != 9001 {
			t.Fatalf("rows = %+v, want account_id 9001", rows)
		}
	})

	t.Run("signed lifecycle event refreshes the account id of a bound row", func(t *testing.T) {
		const installationID = int64(34180003)
		seedAllowlistRow(t, installationID, "old", nil)
		body, _ := json.Marshal(map[string]any{
			"action": "new_permissions_accepted",
			"installation": map[string]any{
				"id": installationID, "account": map[string]any{"id": 9002, "login": "renamed", "type": "Organization"},
			},
		})
		testHandler.handleInstallationEvent(ctx, body)
		rows, _ := testHandler.Queries.ListGitHubInstallationsByInstallationID(ctx, installationID)
		if len(rows) != 1 || !rows[0].AccountID.Valid || rows[0].AccountID.Int64 != 9002 || rows[0].AccountLogin != "renamed" {
			t.Fatalf("rows = %+v, want account_id 9002 login renamed", rows)
		}
	})
}

func TestBackfillGitHubAccountIDs(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	const okID, failID = int64(34180004), int64(34180005)
	seedAllowlistRow(t, okID, "ok-org", nil)
	seedAllowlistRow(t, failID, "fail-org", nil)
	pemBytes, _ := generateTestRSAKeyPEM(t)
	t.Setenv("GITHUB_APP_ID", "268")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", string(pemBytes))
	var mu sync.Mutex
	var usersCalls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/users/"):
			mu.Lock()
			usersCalls = append(usersCalls, r.URL.Path)
			mu.Unlock()
			http.NotFound(w, r)
		case r.URL.Path == fmt.Sprintf("/app/installations/%d", okID):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": okID, "account": map[string]any{"id": 9004, "login": "ok-org", "type": "Organization"},
			})
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	old := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = old })

	resolved, failed := allowlistHandler(t, "").BackfillGitHubAccountIDs(ctx)
	if resolved < 1 || failed < 1 {
		t.Fatalf("resolved=%d failed=%d, want at least one of each", resolved, failed)
	}
	okRows, _ := testHandler.Queries.ListGitHubInstallationsByInstallationID(ctx, okID)
	if len(okRows) != 1 || !okRows[0].AccountID.Valid || okRows[0].AccountID.Int64 != 9004 {
		t.Fatalf("ok row = %+v, want account_id 9004", okRows)
	}
	failRows, _ := testHandler.Queries.ListGitHubInstallationsByInstallationID(ctx, failID)
	if len(failRows) != 1 || failRows[0].AccountID.Valid {
		t.Fatalf("fail row = %+v, want account_id still null", failRows)
	}
	if len(usersCalls) != 0 {
		t.Fatalf("backfill called /users/: %v", usersCalls)
	}
}

// bindFixture serves a verifiable GitHub bind for installationID whose account
// has the given numeric id, then returns a callback request for it.
func bindFixture(t *testing.T, installationID, accountID int64, withCode bool) *http.Request {
	t.Helper()
	pemBytes, _ := generateTestRSAKeyPEM(t)
	t.Setenv("GITHUB_WEBHOOK_SECRET", "allowlist-bind-secret")
	t.Setenv("GITHUB_APP_CLIENT_ID", "allow-client")
	t.Setenv("GITHUB_APP_CLIENT_SECRET", "allow-client-secret")
	t.Setenv("GITHUB_APP_ID", "268")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", string(pemBytes))
	t.Setenv("FRONTEND_ORIGIN", "https://app.example.test")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok"})
		case fmt.Sprintf("/app/installations/%d", installationID):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": installationID, "account": map[string]any{"id": accountID, "login": "owner", "type": "User"},
			})
		case "/user":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": accountID, "login": "owner"})
		case "/applications/allow-client/token":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	oldAPI, oldOAuth := githubAPIBase, githubOAuthBase
	githubAPIBase, githubOAuthBase = srv.URL, srv.URL
	t.Cleanup(func() { githubAPIBase, githubOAuthBase = oldAPI, oldOAuth })

	state, err := signState(testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	target := fmt.Sprintf("/api/github/setup?installation_id=%d&state=%s", installationID, url.QueryEscape(state))
	if withCode {
		target += "&code=c"
	} else {
		target += "&setup_action=update"
	}
	return httptest.NewRequest(http.MethodGet, target, nil)
}

// withTestAllowlist sets the policy on the shared test handler for one test.
func withTestAllowlist(t *testing.T, raw string) *Handler {
	t.Helper()
	old := testHandler.githubAllowlist
	t.Cleanup(func() { testHandler.githubAllowlist = old })
	if err := testHandler.ConfigureGitHubAllowlist(raw); err != nil {
		t.Fatalf("configure allowlist %q: %v", raw, err)
	}
	return testHandler
}

func callSetup(t *testing.T, h *Handler, req *http.Request) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.GitHubSetupCallback(rec, req)
	return rec.Header().Get("Location")
}

func TestSetupCallbackAllowlistBind(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	cases := []struct {
		name      string
		list      string
		accountID int64
		wantErr   bool
		installID int64
	}{
		{"account not in list is refused", "7001", 9001, true, 34130001},
		{"list unset binds", "", 9001, false, 34130002},
		{"account in list binds", "9001,7001", 9001, false, 34130003},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() {
				_, _ = testPool.Exec(ctx, `DELETE FROM github_installation WHERE installation_id = $1`, tc.installID)
			})
			h := withTestAllowlist(t, tc.list)
			loc := callSetup(t, h, bindFixture(t, tc.installID, tc.accountID, true))
			rows, _ := testHandler.Queries.ListGitHubInstallationsByInstallationID(ctx, tc.installID)
			if tc.wantErr {
				if !strings.Contains(loc, "github_error=installation_not_allowed") || len(rows) != 0 {
					t.Fatalf("redirect = %q rows = %d; want installation_not_allowed and no row", loc, len(rows))
				}
				return
			}
			if !strings.Contains(loc, "github_connected=1") || len(rows) != 1 {
				t.Fatalf("redirect = %q rows = %d; want success and one row", loc, len(rows))
			}
		})
	}
}

func TestSetupCallbackAllowlistCodeLessUpdate(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	cases := []struct {
		name    string
		stored  *int64
		wantErr bool
		id      int64
	}{
		{"unlisted stored id is refused", ptr64(9001), true, 34130011},
		{"null stored id is refused", nil, true, 34130012},
		{"listed stored id succeeds", ptr64(7001), false, 34130013},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := seedAllowlistRow(t, tc.id, "stored-login", tc.stored)
			h := withTestAllowlist(t, "7001")
			loc := callSetup(t, h, bindFixture(t, tc.id, 5555, false))
			rows, _ := testHandler.Queries.ListGitHubInstallationsByInstallationID(ctx, tc.id)
			if len(rows) != 1 {
				t.Fatalf("rows = %d, want 1", len(rows))
			}
			if tc.wantErr {
				if !strings.Contains(loc, "github_error=installation_not_allowed") || !rows[0].UpdatedAt.Time.Equal(before.UpdatedAt.Time) {
					t.Fatalf("redirect = %q updated_at changed = %v; want refusal and untouched row",
						loc, !rows[0].UpdatedAt.Time.Equal(before.UpdatedAt.Time))
				}
				return
			}
			if !strings.Contains(loc, "github_connected=1") {
				t.Fatalf("redirect = %q, want success", loc)
			}
		})
	}
}
