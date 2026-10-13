package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestIssueDesktopTokenProvenance(test *testing.T) {
	agentID := createHandlerTestAgent(test, "desktop-token", []byte("[]"))
	protected := middleware.Auth(testHandler.Queries, nil, provenanceCloudVerifier(test), nil)(RequireHumanActor(http.HandlerFunc(testHandler.IssueDesktopToken)))
	for source, token := range provenanceCredentials(test, agentID) {
		for _, transport := range []string{"bearer", "cookie", "cookie_without_csrf"} {
			test.Run(source+"/"+transport, func(test *testing.T) {
				request := newRequest(http.MethodPost, "/api/auth/desktop-token", nil)
				request.Header.Set("X-Actor-Source", "interactive_jwt")
				if transport == "bearer" {
					request.Header.Set("Authorization", "Bearer "+token)
				} else {
					cookieWriter := httptest.NewRecorder()
					if err := auth.SetAuthCookies(cookieWriter, token); err != nil {
						test.Fatal(err)
					}
					for _, cookie := range cookieWriter.Result().Cookies() {
						request.AddCookie(cookie)
						if cookie.Name == auth.CSRFCookieName && transport == "cookie" {
							request.Header.Set(auth.CSRFHeaderName, cookie.Value)
						}
					}
				}
				want := http.StatusForbidden
				if source == "interactive_jwt" && transport != "cookie_without_csrf" {
					want = http.StatusOK
				}
				response := testutil.Call(test, protected.ServeHTTP, request).Want(want)
				if want == http.StatusOK {
					var result map[string]string
					response.JSON(&result)
					claims := provenanceJWTClaims(test, result["token"])
					if claims["amr"] != "interactive" || claims["sub"] != testUserID || claims["sid"] == nil || claims["sid"] == "" {
						test.Fatalf("desktop-token claims = %v", claims)
					}
				}
			})
		}
	}
}

func TestDesktopEmailAndBrowserHandoffReadChat(test *testing.T) {
	agentID := createHandlerTestAgent(test, "desktop-email-access", []byte("[]"))
	sessionID := createHandlerTestChatSession(test, agentID)
	dbfx.Insert(test, "verification_code", testutil.Cols{
		"email": handlerTestEmail, "code": "654321", "expires_at": time.Now().Add(time.Hour),
	})
	var login LoginResponse
	testutil.Call(test, testHandler.VerifyCode, newRequest(http.MethodPost, "/auth/verify-code", map[string]string{
		"email": handlerTestEmail, "code": "654321",
	})).Want(http.StatusOK).JSON(&login)
	if claims := provenanceJWTClaims(test, login.Token); claims["amr"] != "interactive" {
		test.Fatalf("native desktop email-code token claims = %v", claims)
	}
	protected := middleware.Auth(testHandler.Queries, nil, nil, nil)(RequireHumanActor(http.HandlerFunc(testHandler.IssueDesktopToken)))
	for _, path := range []string{"native_desktop_email", "browser_email_handoff", "already_signed_in_handoff"} {
		test.Run(path, func(test *testing.T) {
			token := login.Token
			if path != "native_desktop_email" {
				if path == "already_signed_in_handoff" {
					token = provenanceJWT(test, "interactive")
				}
				cookieWriter := httptest.NewRecorder()
				if err := auth.SetAuthCookies(cookieWriter, token); err != nil {
					test.Fatal(err)
				}
				request := newRequest(http.MethodPost, "/api/auth/desktop-token", nil)
				for _, cookie := range cookieWriter.Result().Cookies() {
					request.AddCookie(cookie)
					if cookie.Name == auth.CSRFCookieName {
						request.Header.Set(auth.CSRFHeaderName, cookie.Value)
					}
				}
				var result map[string]string
				testutil.Call(test, protected.ServeHTTP, request).Want(http.StatusOK).JSON(&result)
				token = result["token"]
				if claims := provenanceJWTClaims(test, token); claims["sid"] == provenanceJWTClaims(test, login.Token)["sid"] {
					test.Fatal("browser desktop handoff reused the original session ID")
				}
			}
			for _, endpoint := range []http.HandlerFunc{
				testHandler.ListChatSessions, testHandler.GetChatSession, testHandler.ListChatMessages, testHandler.ListChatMessagesPage,
			} {
				request := withChatTestWorkspaceCtx(test, newRequest(http.MethodGet, "/api/chat/sessions", nil))
				request = withURLParam(request, "sessionId", sessionID)
				request.Header.Set("Authorization", "Bearer "+token)
				authenticated := middleware.Auth(testHandler.Queries, nil, nil, nil)(endpoint)
				testutil.Call(test, authenticated.ServeHTTP, request).Want(http.StatusOK)
			}
		})
	}
}
