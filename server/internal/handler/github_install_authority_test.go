package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestGitHubConfiguredRequiresAllAuthorityCredentials(t *testing.T) {
	required := map[string]string{
		"GITHUB_APP_SLUG":          "multica-test",
		"GITHUB_WEBHOOK_SECRET":    "webhook-secret",
		"GITHUB_APP_CLIENT_ID":     "client-id",
		"GITHUB_APP_CLIENT_SECRET": "client-secret",
		"GITHUB_APP_ID":            "268",
		"GITHUB_APP_PRIVATE_KEY":   "private-key",
	}
	for name, value := range required {
		t.Setenv(name, value)
	}
	if !isGitHubConfigured() {
		t.Fatal("complete GitHub authority configuration was rejected")
	}
	for missing := range required {
		t.Run("missing "+missing, func(t *testing.T) {
			for name, value := range required {
				t.Setenv(name, value)
			}
			t.Setenv(missing, "")
			if isGitHubConfigured() {
				t.Fatalf("configuration without %s was accepted", missing)
			}
		})
	}
}

func TestAuthorizeGitHubInstallationAccount(t *testing.T) {
	tests := []struct {
		name         string
		installation githubUserInstallation
		userStatus   int
		userBody     string
		memberStatus int
		memberBody   string
		wantErr      bool
	}{
		{
			name:         "personal owner succeeds with matching id and login",
			installation: testGitHubUserInstallation(101, 501, "Personal-Owner", "User"),
			userStatus:   http.StatusOK,
			userBody:     `{"id":501,"login":"personal-owner"}`,
		},
		{
			name:         "personal login mismatch fails closed",
			installation: testGitHubUserInstallation(102, 502, "victim", "User"),
			userStatus:   http.StatusOK,
			userBody:     `{"id":502,"login":"attacker"}`,
			wantErr:      true,
		},
		{
			name:         "personal id mismatch fails closed",
			installation: testGitHubUserInstallation(103, 503, "same-login", "User"),
			userStatus:   http.StatusOK,
			userBody:     `{"id":999,"login":"same-login"}`,
			wantErr:      true,
		},
		{
			name:         "organization admin succeeds",
			installation: testGitHubUserInstallation(201, 601, "Acme-Org", "Organization"),
			userStatus:   http.StatusOK,
			userBody:     `{"id":701,"login":"org-owner"}`,
			memberStatus: http.StatusOK,
			memberBody:   `{"state":"active","role":"admin"}`,
		},
		{
			name:         "ordinary organization member is rejected",
			installation: testGitHubUserInstallation(202, 602, "member-org", "Organization"),
			userStatus:   http.StatusOK,
			userBody:     `{"id":702,"login":"org-member"}`,
			memberStatus: http.StatusOK,
			memberBody:   `{"state":"active","role":"member"}`,
			wantErr:      true,
		},
		{
			name:         "inactive organization admin is rejected",
			installation: testGitHubUserInstallation(203, 603, "inactive-org", "Organization"),
			userStatus:   http.StatusOK,
			userBody:     `{"id":703,"login":"pending-owner"}`,
			memberStatus: http.StatusOK,
			memberBody:   `{"state":"pending","role":"admin"}`,
			wantErr:      true,
		},
		{
			name:         "membership 403 fails closed",
			installation: testGitHubUserInstallation(204, 604, "private-org", "Organization"),
			userStatus:   http.StatusOK,
			userBody:     `{"id":704,"login":"org-owner"}`,
			memberStatus: http.StatusForbidden,
			memberBody:   `{"message":"Resource not accessible"}`,
			wantErr:      true,
		},
		{
			name:         "malformed membership response fails closed",
			installation: testGitHubUserInstallation(205, 605, "broken-org", "Organization"),
			userStatus:   http.StatusOK,
			userBody:     `{"id":705,"login":"org-owner"}`,
			memberStatus: http.StatusOK,
			memberBody:   `not-json`,
			wantErr:      true,
		},
		{
			name:         "malformed user response fails closed",
			installation: testGitHubUserInstallation(301, 801, "personal", "User"),
			userStatus:   http.StatusOK,
			userBody:     `not-json`,
			wantErr:      true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/user":
					w.WriteHeader(tc.userStatus)
					_, _ = w.Write([]byte(tc.userBody))
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/user/memberships/orgs/"):
					w.WriteHeader(tc.memberStatus)
					_, _ = w.Write([]byte(tc.memberBody))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			oldAPIBase := githubAPIBase
			githubAPIBase = srv.URL
			t.Cleanup(func() { githubAPIBase = oldAPIBase })

			err := authorizeGitHubInstallationAccount(
				context.Background(),
				srv.Client(),
				"ghu_test_user_token",
				tc.installation,
			)
			if (err != nil) != tc.wantErr {
				t.Fatalf("authorizeGitHubInstallationAccount() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestFetchGitHubInstallationForBindingIsAuthoritative(t *testing.T) {
	pemBytes, _ := generateTestRSAKeyPEM(t)
	t.Setenv("GITHUB_APP_ID", "12345")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", string(pemBytes))

	tests := []struct {
		name       string
		status     int
		body       any
		wantErr    bool
		wantLogin  string
		wantInstID int64
	}{
		{
			name:       "returns the App-authorized installation",
			status:     http.StatusOK,
			body:       map[string]any{"id": 401, "account": map[string]any{"id": 901, "login": "acme", "type": "Organization"}},
			wantLogin:  "acme",
			wantInstID: 401,
		},
		{
			name:    "GitHub 403 fails closed",
			status:  http.StatusForbidden,
			body:    map[string]any{"message": "forbidden"},
			wantErr: true,
		},
		{
			name:    "malformed response fails closed",
			status:  http.StatusOK,
			body:    "not-an-installation",
			wantErr: true,
		},
		{
			name:    "different installation id fails closed",
			status:  http.StatusOK,
			body:    map[string]any{"id": 402, "account": map[string]any{"id": 901, "login": "acme", "type": "Organization"}},
			wantErr: true,
		},
		{
			name:    "missing account id fails closed",
			status:  http.StatusOK,
			body:    map[string]any{"id": 401, "account": map[string]any{"login": "acme", "type": "Organization"}},
			wantErr: true,
		},
		{
			name:    "unsupported account type fails closed",
			status:  http.StatusOK,
			body:    map[string]any{"id": 401, "account": map[string]any{"id": 901, "login": "acme", "type": "Enterprise"}},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
					http.Error(w, "missing App JWT", http.StatusUnauthorized)
					return
				}
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(tc.body)
			}))
			t.Cleanup(srv.Close)
			oldAPIBase := githubAPIBase
			githubAPIBase = srv.URL
			t.Cleanup(func() { githubAPIBase = oldAPIBase })

			installation, err := fetchGitHubInstallationForBinding(
				context.Background(),
				srv.Client(),
				401,
			)
			if (err != nil) != tc.wantErr {
				t.Fatalf("fetchGitHubInstallationForBinding() error = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && (installation.ID != tc.wantInstID || installation.Account.Login != tc.wantLogin) {
				t.Fatalf("installation = %+v, want id %d login %q", installation, tc.wantInstID, tc.wantLogin)
			}
		})
	}
}

func TestExchangeGitHubUserCodeFailsClosed(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantToken string
		wantErr   bool
	}{
		{name: "valid token", status: http.StatusOK, body: `{"access_token":"ghu_verified"}`, wantToken: "ghu_verified"},
		{name: "GitHub status error", status: http.StatusUnauthorized, body: `{"error":"bad_verification_code"}`, wantErr: true},
		{name: "OAuth error response", status: http.StatusOK, body: `{"error":"bad_verification_code"}`, wantErr: true},
		{name: "malformed response", status: http.StatusOK, body: `not-json`, wantErr: true},
		{name: "empty token", status: http.StatusOK, body: `{}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/login/oauth/access_token" {
					http.NotFound(w, r)
					return
				}
				if err := r.ParseForm(); err != nil || r.Form.Get("code") != "authorization-code" {
					http.Error(w, "bad form", http.StatusBadRequest)
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			oldOAuthBase := githubOAuthBase
			githubOAuthBase = srv.URL
			t.Cleanup(func() { githubOAuthBase = oldOAuthBase })

			token, err := exchangeGitHubUserCode(
				context.Background(), srv.Client(), "client-id", "client-secret", "authorization-code",
			)
			if (err != nil) != tc.wantErr {
				t.Fatalf("exchangeGitHubUserCode() error = %v, wantErr %v", err, tc.wantErr)
			}
			if token != tc.wantToken {
				t.Fatalf("exchangeGitHubUserCode() token = %q, want %q", token, tc.wantToken)
			}
		})
	}
}

func TestSetupCallbackRejectsCodeLessNewBinding(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	const installationID = int64(82680103)
	t.Setenv("GITHUB_WEBHOOK_SECRET", "code-less-new-binding-secret")
	t.Setenv("FRONTEND_ORIGIN", "https://app.example.test")
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM github_installation WHERE installation_id = $1`, installationID)
	})

	state, err := signState(testWorkspaceID)
	if err != nil {
		t.Fatalf("sign state: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf(
		"/api/github/setup?installation_id=%d&setup_action=update&state=%s",
		installationID,
		url.QueryEscape(state),
	), nil)
	rec := httptest.NewRecorder()
	testHandler.GitHubSetupCallback(rec, req)
	if location := rec.Header().Get("Location"); !strings.Contains(location, "github_error=missing_authorization") {
		t.Fatalf("redirect = %q, want missing_authorization", location)
	}
	rows, err := testHandler.Queries.ListGitHubInstallationsByInstallationID(ctx, installationID)
	if err != nil {
		t.Fatalf("list rejected installation: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("code-less new callback created %d binding(s)", len(rows))
	}
}

func TestSetupCallbackRejectsSubstitutedInstallation(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	const substitutedInstallationID = int64(82680101)
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM github_installation WHERE installation_id = $1`, substitutedInstallationID)
	})

	pemBytes, _ := generateTestRSAKeyPEM(t)
	t.Setenv("GITHUB_WEBHOOK_SECRET", "substituted-installation-secret")
	t.Setenv("GITHUB_APP_CLIENT_ID", "substitution-client")
	t.Setenv("GITHUB_APP_CLIENT_SECRET", "substitution-secret")
	t.Setenv("GITHUB_APP_ID", "268")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", string(pemBytes))
	t.Setenv("FRONTEND_ORIGIN", "https://app.example.test")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "attacker-token"})
		case fmt.Sprintf("/app/installations/%d", substitutedInstallationID):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":      substitutedInstallationID,
				"account": map[string]any{"id": 3001, "login": "victim", "type": "User"},
			})
		case "/user":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 3002, "login": "attacker"})
		case "/applications/substitution-client/token":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	oldAPIBase, oldOAuthBase := githubAPIBase, githubOAuthBase
	githubAPIBase, githubOAuthBase = srv.URL, srv.URL
	t.Cleanup(func() { githubAPIBase, githubOAuthBase = oldAPIBase, oldOAuthBase })

	state, err := signState(testWorkspaceID)
	if err != nil {
		t.Fatalf("sign state: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf(
		"/api/github/setup?installation_id=%d&code=attacker-code&state=%s",
		substitutedInstallationID,
		url.QueryEscape(state),
	), nil)
	rec := httptest.NewRecorder()
	testHandler.GitHubSetupCallback(rec, req)
	if location := rec.Header().Get("Location"); !strings.Contains(location, "github_error=installation_not_authorized") {
		t.Fatalf("redirect = %q, want installation_not_authorized", location)
	}
	rows, err := testHandler.Queries.ListGitHubInstallationsByInstallationID(ctx, substitutedInstallationID)
	if err != nil {
		t.Fatalf("list substituted installation: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("substituted installation created %d binding(s)", len(rows))
	}
}

func TestSetupCallbackCodeLessUpdatePreservesAttribution(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	const installationID = int64(82680102)
	t.Setenv("GITHUB_WEBHOOK_SECRET", "code-less-update-secret")
	t.Setenv("FRONTEND_ORIGIN", "https://app.example.test")
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM github_installation WHERE installation_id = $1`, installationID)
	})

	created, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		InstallationID: installationID,
		AccountLogin:   "existing-org",
		AccountType:    "Organization",
		ConnectedByID:  parseUUID(testUserID),
	})
	if err != nil {
		t.Fatalf("seed installation: %v", err)
	}
	state, err := signState(testWorkspaceID)
	if err != nil {
		t.Fatalf("sign state: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf(
		"/api/github/setup?installation_id=%d&setup_action=update&state=%s",
		installationID,
		url.QueryEscape(state),
	), nil)
	rec := httptest.NewRecorder()
	testHandler.GitHubSetupCallback(rec, req)
	if location := rec.Header().Get("Location"); !strings.Contains(location, "github_connected=1") {
		t.Fatalf("code-less update redirect = %q, want success", location)
	}
	rows, err := testHandler.Queries.ListGitHubInstallationsByInstallationID(ctx, installationID)
	if err != nil {
		t.Fatalf("list updated installation: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != created.ID || rows[0].ConnectedByID != parseUUID(testUserID) {
		t.Fatalf("code-less update changed row identity or attribution: %+v", rows)
	}
}

func TestGitHubInstallStateExpiryAndTamper(t *testing.T) {
	t.Setenv("GITHUB_WEBHOOK_SECRET", "state-ttl-secret")
	const workspaceID = "11111111-2222-3333-4444-555555555555"
	now := time.Now()

	fresh, err := signStateForReturnAt(workspaceID, githubReturnToRepositories, now)
	if err != nil {
		t.Fatalf("sign fresh state: %v", err)
	}
	if gotWorkspace, gotReturn, ok := verifyStateWithReturnAt(fresh, now); !ok || gotWorkspace != workspaceID || gotReturn != githubReturnToRepositories {
		t.Fatalf("fresh state changed or rejected: (%q, %q, %v)", gotWorkspace, gotReturn, ok)
	}

	tampered := strings.Replace(fresh, ".repositories.", ".github.", 1)
	if _, _, ok := verifyStateWithReturnAt(tampered, now); ok {
		t.Fatal("tampered state was accepted")
	}
	stale, err := signStateForReturnAt(workspaceID, githubReturnToGitHub, now.Add(-githubStateMaxAge-time.Second))
	if err != nil {
		t.Fatalf("sign stale state: %v", err)
	}
	if _, _, ok := verifyStateWithReturnAt(stale, now); ok {
		t.Fatal("stale state was accepted")
	}
	future, err := signStateForReturnAt(workspaceID, githubReturnToGitHub, now.Add(githubStateClockSkew+time.Second))
	if err != nil {
		t.Fatalf("sign future state: %v", err)
	}
	if _, _, ok := verifyStateWithReturnAt(future, now); ok {
		t.Fatal("state beyond clock skew was accepted")
	}
}

func testGitHubUserInstallation(installationID, accountID int64, login, accountType string) githubUserInstallation {
	installation := githubUserInstallation{ID: installationID}
	installation.Account.ID = accountID
	installation.Account.Login = login
	installation.Account.Type = accountType
	return installation
}
