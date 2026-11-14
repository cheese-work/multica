package handler

import (
	"context"
	"encoding/json"
	"github.com/multica-ai/multica/server/internal/integrations/lark"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	dingtalkintegration "github.com/multica-ai/multica/server/internal/integrations/dingtalk"
	slackintegration "github.com/multica-ai/multica/server/internal/integrations/slack"
	telegramintegration "github.com/multica-ai/multica/server/internal/integrations/telegram"
	"github.com/multica-ai/multica/server/internal/integrations/wecom"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
)

// CHE-5650 (E6 of CHE-4807): chat-integration install, uninstall and DingTalk
// group-forget routes write one workspace_audit_event row in the mutation's own
// transaction. Each route has a success test (one row, kind, member actor,
// target id and type, no credential in before/after) and a rollback test
// (forced audit insert failure -> 500 + Retry-After, mutation not applied).

const chatSecret = "SECRET-CHAT-CREDENTIAL"

func chatTestBox(t *testing.T) *secretbox.Box {
	t.Helper()
	box, err := secretbox.New(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	return box
}

// wireChatServices attaches the Slack, Telegram, DingTalk and WeCom install
// services to the shared handler, restoring the previous values on cleanup.
func wireChatServices(t *testing.T) {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	box := chatTestBox(t)
	slackSvc, err := slackintegration.NewInstallService(testHandler.Queries, testPool, box, nil)
	if err != nil {
		t.Fatalf("slack NewInstallService: %v", err)
	}
	tgSvc, err := telegramintegration.NewInstallService(testHandler.Queries, testPool, box, nil)
	if err != nil {
		t.Fatalf("telegram NewInstallService: %v", err)
	}
	dtSvc, err := dingtalkintegration.NewInstallService(testHandler.Queries, testPool, box, nil)
	if err != nil {
		t.Fatalf("dingtalk NewInstallService: %v", err)
	}
	creds, err := wecom.NewSecretboxCredentialsResolver(box)
	if err != nil {
		t.Fatalf("wecom credentials resolver: %v", err)
	}
	h := testHandler
	prevSlack, prevTG, prevDT := h.SlackInstall, h.TelegramInstall, h.DingTalkInstall
	prevStore, prevCreds, prevRouter := h.WecomStore, h.WecomCredentials, h.ChannelRouter
	h.SlackInstall, h.TelegramInstall, h.DingTalkInstall = slackSvc, tgSvc, dtSvc
	h.WecomStore, h.WecomCredentials, h.ChannelRouter = wecom.NewStore(h.Queries), creds, &engine.Router{}
	t.Cleanup(func() {
		h.SlackInstall, h.TelegramInstall, h.DingTalkInstall = prevSlack, prevTG, prevDT
		h.WecomStore, h.WecomCredentials, h.ChannelRouter = prevStore, prevCreds, prevRouter
	})
}

// seedChatInstallation inserts an active installation whose config holds a
// credential, for an agent of its own so repeated cases never collide.
func seedChatInstallation(t *testing.T, channelType string) (installationID, agentID string) {
	t.Helper()
	agentID = createHandlerTestAgent(t, "audit-chat-"+channelType+"-"+t.Name(), nil)
	installationID = dbfx.Insert(t, "channel_installation", testutil.Cols{
		"workspace_id":      testWorkspaceID,
		"agent_id":          agentID,
		"channel_type":      channelType,
		"config":            []byte(`{"app_id":"audit-` + channelType + `","bot_token_encrypted":"` + chatSecret + `"}`),
		"installer_user_id": testUserID,
		"status":            "active",
	})
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM channel_installation WHERE id = $1`, installationID)
	})
	return installationID, agentID
}

func installationStatus(t *testing.T, installationID string) string {
	t.Helper()
	var status string
	if err := testPool.QueryRow(context.Background(),
		`SELECT status FROM channel_installation WHERE id = $1`, installationID).Scan(&status); err != nil {
		t.Fatalf("read installation status: %v", err)
	}
	return status
}

type chatRevokeCase struct {
	provider    string
	channelType string
	wire        func(*testing.T)
	handler     func(*Handler) http.HandlerFunc
	route       string
}

func chatRevokeCases() []chatRevokeCase {
	return []chatRevokeCase{
		{"slack", "slack", wireChatServices, func(h *Handler) http.HandlerFunc { return h.RevokeSlackInstallation }, "slack"},
		{"telegram", "telegram", wireChatServices, func(h *Handler) http.HandlerFunc { return h.RevokeTelegramInstallation }, "telegram"},
		{"wecom", "wecom", wireChatServices, func(h *Handler) http.HandlerFunc { return h.RevokeWecomInstallation }, "wecom"},
		{"dingtalk", "dingtalk", wireChatServices, func(h *Handler) http.HandlerFunc { return h.RevokeDingTalkInstallation }, "dingtalk"},
		{"lark", "feishu", wireLarkInstallServices, func(h *Handler) http.HandlerFunc { return h.RevokeLarkInstallation }, "lark"},
	}
}

func revokeChatInstallation(tc chatRevokeCase, installationID string) *httptest.ResponseRecorder {
	router := chi.NewRouter()
	router.Delete("/api/workspaces/{id}/"+tc.route+"/installations/{installationId}", tc.handler(testHandler))
	req := newRequestAs(testUserID, http.MethodDelete,
		"/api/workspaces/"+testWorkspaceID+"/"+tc.route+"/installations/"+installationID, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestChatIntegrationRevoke_RecordsAuditRow(t *testing.T) {
	for _, tc := range chatRevokeCases() {
		t.Run(tc.provider, func(t *testing.T) {
			tc.wire(t)
			installationID, _ := seedChatInstallation(t, tc.channelType)

			w := revokeChatInstallation(tc, installationID)
			if w.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204: %s", w.Code, w.Body.String())
			}
			row := onlyAuditRow(t, tc.provider+".uninstall", installationID, "channel_installation")
			if row.Before["status"] != "active" || row.After["status"] != "revoked" {
				t.Errorf("before/after = %v / %v; want status active -> revoked", row.Before, row.After)
			}
			assertNoneStored(t, row, chatSecret, "bot_token", "secret")
		})
	}
}

func TestChatIntegrationRevoke_AuditInsertFailureKeepsInstallation(t *testing.T) {
	for _, tc := range chatRevokeCases() {
		t.Run(tc.provider, func(t *testing.T) {
			tc.wire(t)
			installationID, _ := seedChatInstallation(t, tc.channelType)
			failSquadSkillAuditForKind(t, tc.provider+".uninstall", testWorkspaceID)

			assertAuditFailure(t, revokeChatInstallation(tc, installationID))
			if got := installationStatus(t, installationID); got != "active" {
				t.Errorf("installation status = %q; want active (revoke must roll back)", got)
			}
		})
	}
}

// ── DingTalk group forget ───────────────────────────────────────────────────

func seedDingTalkGroup(t *testing.T) (installationID, conversationID string) {
	t.Helper()
	installationID, _ = seedChatInstallation(t, "dingtalk")
	conversationID = "cid/audit-forget"
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO dingtalk_group_presence (workspace_id, installation_id, conversation_id, conversation_title)
		VALUES ($1, $2, $3, 'SECRET-GROUP-TITLE')`, testWorkspaceID, installationID, conversationID); err != nil {
		t.Fatalf("seed group presence: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM dingtalk_group_presence WHERE installation_id = $1`, installationID)
	})
	return installationID, conversationID
}

func forgetDingTalkGroup(installationID string) *httptest.ResponseRecorder {
	router := chi.NewRouter()
	router.Delete("/api/workspaces/{id}/dingtalk/installations/{installationId}/groups/{conversationId}",
		testHandler.ForgetDingTalkGroup)
	req := newRequestAs(testUserID, http.MethodDelete,
		"/api/workspaces/"+testWorkspaceID+"/dingtalk/installations/"+installationID+"/groups/cid%2Faudit-forget", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func groupPresenceCount(t *testing.T, installationID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM dingtalk_group_presence WHERE installation_id = $1`, installationID)
}

func TestForgetDingTalkGroup_RecordsAuditRow(t *testing.T) {
	wireChatServices(t)
	installationID, conversationID := seedDingTalkGroup(t)

	if w := forgetDingTalkGroup(installationID); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", w.Code, w.Body.String())
	}
	row := onlyAuditRow(t, "dingtalk.group.forget", installationID, "channel_installation")
	if row.Before["conversation_id"] != conversationID {
		t.Errorf("before = %v; want conversation_id %q", row.Before, conversationID)
	}
	assertNoneStored(t, row, "SECRET-GROUP-TITLE", chatSecret)
}

func TestForgetDingTalkGroup_AuditInsertFailureKeepsGroup(t *testing.T) {
	wireChatServices(t)
	installationID, _ := seedDingTalkGroup(t)
	failSquadSkillAuditForKind(t, "dingtalk.group.forget", testWorkspaceID)

	assertAuditFailure(t, forgetDingTalkGroup(installationID))
	if n := groupPresenceCount(t, installationID); n != 1 {
		t.Errorf("group presence rows = %d; want 1 (forget must roll back)", n)
	}
}

// ── installs ────────────────────────────────────────────────────────────────

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// redirectOutboundHTTP sends every request made through http.DefaultTransport
// (Slack, Telegram and DingTalk credential checks) to a fake server, keeping
// the request path so the fake can answer per API method.
func redirectOutboundHTTP(t *testing.T, fake http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(fake)
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse fake url: %v", err)
	}
	prev := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r2 := r.Clone(r.Context())
		r2.URL.Scheme, r2.URL.Host, r2.Host = target.Scheme, target.Host, target.Host
		return prev.RoundTrip(r2)
	})
	t.Cleanup(func() {
		http.DefaultTransport = prev
		srv.Close()
	})
}

func fakeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// fakeChatAPIs answers the credential checks of Slack, Telegram and DingTalk.
func fakeChatAPIs(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/auth.test"):
		fakeJSON(w, map[string]any{"ok": true, "team_id": "T1", "user_id": "U1", "bot_id": "B1"})
	case strings.HasSuffix(r.URL.Path, "/bots.info"):
		fakeJSON(w, map[string]any{"ok": true, "bot": map[string]any{"app_id": "A0AUDIT", "id": "B1"}})
	case strings.HasSuffix(r.URL.Path, "/apps.connections.open"):
		fakeJSON(w, map[string]any{"ok": true, "url": "wss://example.invalid/link"})
	case strings.HasSuffix(r.URL.Path, "/getMe"):
		fakeJSON(w, map[string]any{"ok": true, "result": map[string]any{"id": 4242, "is_bot": true, "username": "audit_bot"}})
	case strings.HasSuffix(r.URL.Path, "/getWebhookInfo"):
		fakeJSON(w, map[string]any{"ok": true, "result": map[string]any{"url": ""}})
	case strings.HasSuffix(r.URL.Path, "/v1.0/oauth2/accessToken"):
		fakeJSON(w, map[string]any{"accessToken": "audit-access-token", "expireIn": 7200})
	default:
		http.NotFound(w, r)
	}
}

type okProbe struct{}

func (okProbe) Probe(context.Context, string, string) error { return nil }

type chatInstallCase struct {
	provider string
	wire     func(*testing.T)
	path     string
	handler  func(*Handler) http.HandlerFunc
	body     map[string]any
	secrets  []string
}

func chatInstallCases() []chatInstallCase {
	return []chatInstallCase{
		{"slack", wireChatServices, "slack/install/byo",
			func(h *Handler) http.HandlerFunc { return h.RegisterSlackBYO },
			map[string]any{"bot_token": "xoxb-SECRET-BOT-TOKEN", "app_token": "xapp-1-A0AUDIT-1-SECRET-APP-TOKEN"}, // gitleaks:allow
			[]string{"SECRET-BOT-TOKEN", "SECRET-APP-TOKEN", "xoxb-", "xapp-"}},
		{"telegram", wireChatServices, "telegram/install",
			func(h *Handler) http.HandlerFunc { return h.RegisterTelegramBot },
			map[string]any{"bot_token": "4242:SECRET-TELEGRAM-TOKEN"},
			[]string{"SECRET-TELEGRAM-TOKEN"}},
		{"wecom", wireChatServices, "wecom/install/byo",
			func(h *Handler) http.HandlerFunc { return h.RegisterWecomBYO },
			map[string]any{"bot_id": "audit-wecom-bot", "secret": "SECRET-WECOM-SECRET"},
			[]string{"SECRET-WECOM-SECRET"}},
		{"dingtalk", wireChatServices, "dingtalk/install/byo",
			func(h *Handler) http.HandlerFunc { return h.RegisterDingTalkBYO },
			map[string]any{"client_id": "audit-dingtalk-key", "client_secret": "SECRET-DINGTALK-SECRET"},
			[]string{"SECRET-DINGTALK-SECRET"}},
	}
}

func installChat(t *testing.T, tc chatInstallCase, agentID string) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Post("/api/workspaces/{id}/"+tc.path, tc.handler(testHandler))
	req := newRequestAs(testUserID, http.MethodPost,
		"/api/workspaces/"+testWorkspaceID+"/"+tc.path+"?agent_id="+agentID, tc.body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func prepareChatInstall(t *testing.T, tc chatInstallCase) (agentID string) {
	t.Helper()
	tc.wire(t)
	redirectOutboundHTTP(t, fakeChatAPIs)
	prevProbe := testHandler.WecomCredentialProbe
	testHandler.WecomCredentialProbe = okProbe{}
	t.Cleanup(func() { testHandler.WecomCredentialProbe = prevProbe })
	agentID = createHandlerTestAgent(t, "audit-chat-install-"+tc.provider, nil)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM channel_installation WHERE agent_id = $1`, agentID)
	})
	return agentID
}

func installationCountForAgent(t *testing.T, agentID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM channel_installation WHERE agent_id = $1`, agentID)
}

func TestChatIntegrationInstall_RecordsAuditRow(t *testing.T) {
	for _, tc := range chatInstallCases() {
		t.Run(tc.provider, func(t *testing.T) {
			agentID := prepareChatInstall(t, tc)

			w := installChat(t, tc, agentID)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
			}
			var resp struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.ID == "" {
				t.Fatalf("decode installation id: %v (%s)", err, w.Body.String())
			}
			row := onlyAuditRow(t, tc.provider+".install", resp.ID, "channel_installation")
			if row.After["provider"] != tc.provider || row.After["agent_id"] != agentID || row.After["status"] != "active" {
				t.Errorf("after = %v; want provider %s, agent_id %s, status active", row.After, tc.provider, agentID)
			}
			assertNoneStored(t, row, append(tc.secrets, "bot_token", "secret", chatSecret)...)
		})
	}
}

func TestChatIntegrationInstall_AuditInsertFailureInstallsNothing(t *testing.T) {
	for _, tc := range chatInstallCases() {
		t.Run(tc.provider, func(t *testing.T) {
			agentID := prepareChatInstall(t, tc)
			failSquadSkillAuditForKind(t, tc.provider+".install", testWorkspaceID)

			assertAuditFailure(t, installChat(t, tc, agentID))
			if n := installationCountForAgent(t, agentID); n != 0 {
				t.Errorf("installation rows for agent = %d; want 0 (install must roll back)", n)
			}
		})
	}
}

// ── Lark install (audited where the device flow completes) ─────────────────

// wireLarkCompletingFlow wires the Lark services against fakes whose device
// flow completes on the first poll: the begin call returns a device code, the
// poll returns app credentials, and the open-platform fake answers token mint
// and bot info.
func wireLarkCompletingFlow(t *testing.T) {
	t.Helper()
	if testHandler == nil {
		t.Skip("database not available")
	}
	box := chatTestBox(t)
	installSvc, err := lark.NewInstallationService(testHandler.Queries, box)
	if err != nil {
		t.Fatalf("NewInstallationService: %v", err)
	}
	device := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("action") == "begin" {
			fakeJSON(w, map[string]any{
				"device_code": "dc_audit", "verification_uri_complete": "https://accounts.feishu.cn/qr?c=1",
				"verification_uri": "https://accounts.feishu.cn/qr", "user_code": "AUDIT-1", "interval": 1, "expire_in": 60,
			})
			return
		}
		fakeJSON(w, map[string]any{
			"client_id": "cli_audit_install", "client_secret": larkAppSecret,
			"user_info": map[string]any{"open_id": "ou_audit_installer", "tenant_brand": "feishu"},
		})
	}))
	t.Cleanup(device.Close)
	open := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/tenant_access_token/internal"):
			fakeJSON(w, map[string]any{"code": 0, "tenant_access_token": "t-audit", "expire": 7200})
		case strings.HasSuffix(r.URL.Path, "/bot/v3/info"):
			fakeJSON(w, map[string]any{"code": 0, "bot": map[string]any{"open_id": "ou_audit_bot"}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(open.Close)

	regSvc, err := lark.NewRegistrationService(
		lark.RegistrationServiceConfig{AuditInstall: testHandler.LarkInstallAuditHook()},
		lark.NewRegistrationClient(lark.RegistrationConfig{Domain: device.URL}),
		lark.NewHTTPAPIClient(lark.HTTPClientConfig{BaseURL: open.URL}),
		testHandler.Queries, testPool, installSvc,
		lark.NewBindingTokenService(testHandler.Queries, testPool),
	)
	if err != nil {
		t.Fatalf("NewRegistrationService: %v", err)
	}
	prevInstall, prevReg := testHandler.LarkInstallations, testHandler.LarkRegistration
	testHandler.LarkInstallations, testHandler.LarkRegistration = installSvc, regSvc
	t.Cleanup(func() { testHandler.LarkInstallations, testHandler.LarkRegistration = prevInstall, prevReg })
}

const larkAppSecret = "SECRET-LARK-APP-SECRET"

// runLarkInstall begins an install and waits for the background device flow
// to finish, returning the terminal status and installation id.
func runLarkInstall(t *testing.T, agentID string) (status, installationID string) {
	t.Helper()
	begin := beginLarkInstallAs(testUserID, agentID)
	if begin.Code != http.StatusOK {
		t.Fatalf("begin status = %d: %s", begin.Code, begin.Body.String())
	}
	var started BeginLarkInstallResponse
	if err := json.Unmarshal(begin.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode begin: %v", err)
	}
	router := chi.NewRouter()
	router.Get("/api/workspaces/{id}/lark/install/{sessionId}/status", testHandler.GetLarkInstallStatus)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, newRequestAs(testUserID, http.MethodGet,
			"/api/workspaces/"+testWorkspaceID+"/lark/install/"+started.SessionID+"/status", nil))
		var st LarkInstallStatusResponse
		if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
			t.Fatalf("decode status: %v (%s)", err, w.Body.String())
		}
		if st.Status != "pending" {
			return st.Status, st.InstallationID
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("lark install did not finish within 15s")
	return "", ""
}

func prepareLarkInstall(t *testing.T) (agentID string) {
	t.Helper()
	wireLarkCompletingFlow(t)
	agentID = createHandlerTestAgent(t, "audit-chat-install-lark", nil)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM channel_installation WHERE agent_id = $1`, agentID)
	})
	return agentID
}

func TestLarkInstall_RecordsAuditRow(t *testing.T) {
	agentID := prepareLarkInstall(t)

	status, installationID := runLarkInstall(t, agentID)
	if status != "success" || installationID == "" {
		t.Fatalf("install status = %q id %q; want success", status, installationID)
	}
	row := onlyAuditRow(t, "lark.install", installationID, "channel_installation")
	if row.After["provider"] != "lark" || row.After["agent_id"] != agentID || row.After["status"] != "active" {
		t.Errorf("after = %v; want provider lark, agent_id %s, status active", row.After, agentID)
	}
	assertNoneStored(t, row, larkAppSecret, "app_secret", "secret")
}

func TestLarkInstall_AuditInsertFailureInstallsNothing(t *testing.T) {
	agentID := prepareLarkInstall(t)
	failSquadSkillAuditForKind(t, "lark.install", testWorkspaceID)

	status, _ := runLarkInstall(t, agentID)
	if status == "success" {
		t.Errorf("install status = success; want an error when the audit insert fails")
	}
	if n := installationCountForAgent(t, agentID); n != 0 {
		t.Errorf("installation rows for agent = %d; want 0 (install must roll back)", n)
	}
}
