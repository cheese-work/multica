package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/redis/go-redis/v9"
)

func provenanceJWT(test *testing.T, amr string) string {
	test.Helper()
	claims := jwt.MapClaims{"sub": testUserID, "exp": time.Now().Add(time.Hour).Unix()}
	if amr != "" {
		claims["amr"] = amr
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(auth.JWTSecret())
	if err != nil {
		test.Fatal(err)
	}
	return token
}

func provenanceJWTClaims(test *testing.T, tokenString string) jwt.MapClaims {
	test.Helper()
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (any, error) { return auth.JWTSecret(), nil }, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		test.Fatal(err)
	}
	return token.Claims.(jwt.MapClaims)
}

func provenanceCredentials(test *testing.T, agentID string) map[string]string {
	test.Helper()
	taskID := dbfx.Task(test, agentID, testutil.Cols{
		"status": "running", "runtime_id": handlerTestRuntimeID(test),
	})
	taskToken := "mat_provenance_" + taskID
	dbfx.Insert(test, "task_token", testutil.Cols{
		"token_hash": auth.HashToken(taskToken), "task_id": taskID, "agent_id": agentID,
		"workspace_id": testWorkspaceID, "user_id": testUserID, "expires_at": time.Now().Add(time.Hour),
	})
	patToken := "mul_provenance_" + taskID
	dbfx.Insert(test, "personal_access_token", testutil.Cols{
		"user_id": testUserID, "name": "provenance", "token_hash": auth.HashToken(patToken), "token_prefix": "mul_prov",
	})
	return map[string]string{
		"interactive_jwt": provenanceJWT(test, "interactive"), "cli_jwt": provenanceJWT(test, "cli"),
		"legacy_jwt": provenanceJWT(test, ""), "unknown_jwt": provenanceJWT(test, "other"),
		"task_token": taskToken, "pat": patToken, "cloud_pat": "mcn_provenance",
	}
}

func provenanceCloudVerifier(test *testing.T) *auth.CloudPATVerifier {
	test.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(writer, `{"valid":true,"owner_id":%q}`, testUserID)
	}))
	test.Cleanup(server.Close)
	return auth.NewCloudPATVerifier(auth.CloudPATVerifierConfig{FleetBaseURL: server.URL})
}

func TestChatMessageAuthProvenance(test *testing.T) {
	agentID := createHandlerTestAgent(test, "AuthProvenance", []byte("[]"))
	credentials := provenanceCredentials(test, agentID)
	verifier := provenanceCloudVerifier(test)
	for source, token := range credentials {
		test.Run(source, func(test *testing.T) {
			sessionID := createHandlerTestChatSession(test, agentID)
			request := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/messages", map[string]string{
				"content": "auth provenance", "sent_via": "interactive_jwt", "amr": "interactive",
			})
			request = withChatTestWorkspaceCtx(test, withURLParam(request, "sessionId", sessionID))
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("X-Actor-Source", "interactive_jwt")
			var response SendChatMessageResponse
			protected := middleware.Auth(testHandler.Queries, nil, verifier)(http.HandlerFunc(testHandler.SendChatMessage))
			testutil.Call(test, protected.ServeHTTP, request).Want(http.StatusCreated).JSON(&response)
			var sentVia string
			if err := testPool.QueryRow(context.Background(), `SELECT sent_via FROM chat_message WHERE id = $1`, response.MessageID).Scan(&sentVia); err != nil {
				test.Fatal(err)
			}
			want := source
			if source == "legacy_jwt" || source == "unknown_jwt" {
				want = "cli_jwt"
			}
			if sentVia != want {
				test.Fatalf("persisted provenance = %q, want %q", sentVia, want)
			}
		})
	}
}

func TestIssueCliTokenProvenance(test *testing.T) {
	agentID := createHandlerTestAgent(test, "CliTokenProvenance", []byte("[]"))
	credentials := provenanceCredentials(test, agentID)
	protected := middleware.Auth(testHandler.Queries, nil, provenanceCloudVerifier(test))(RequireHumanActor(http.HandlerFunc(testHandler.IssueCliToken)))
	for source, token := range credentials {
		test.Run(source, func(test *testing.T) {
			request := newRequest(http.MethodPost, "/api/cli-token", nil)
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("X-Actor-Source", "interactive_jwt")
			if source != "interactive_jwt" {
				testutil.Call(test, protected.ServeHTTP, request).Want(http.StatusForbidden)
				return
			}
			var response map[string]string
			testutil.Call(test, protected.ServeHTTP, request).Want(http.StatusOK).JSON(&response)
			claims := provenanceJWTClaims(test, response["token"])
			if claims["amr"] != "cli" || claims["sub"] != testUserID {
				test.Fatalf("CLI token claims = %v", claims)
			}
			request = newRequest(http.MethodPost, "/api/cli-token", nil)
			request.Header.Set("Authorization", "Bearer "+response["token"])
			request.Header.Set("X-Actor-Source", "interactive_jwt")
			testutil.Call(test, protected.ServeHTTP, request).Want(http.StatusForbidden)
		})
	}
}

func TestChatCookieAuthProvenance(test *testing.T) {
	agentID := createHandlerTestAgent(test, "CookieAuthProvenance", []byte("[]"))
	for _, method := range []string{"interactive", "cli"} {
		test.Run(method, func(test *testing.T) {
			token := provenanceJWT(test, method)
			cookies := httptest.NewRecorder()
			if err := auth.SetAuthCookies(cookies, token); err != nil {
				test.Fatal(err)
			}
			sessionID := createHandlerTestChatSession(test, agentID)
			request := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/messages", map[string]string{
				"content": "cookie provenance", "sent_via": "interactive_jwt",
			})
			request = withChatTestWorkspaceCtx(test, withURLParam(request, "sessionId", sessionID))
			request.Header.Set("X-Actor-Source", "interactive_jwt")
			for _, cookie := range cookies.Result().Cookies() {
				request.AddCookie(cookie)
				if cookie.Name == auth.CSRFCookieName {
					request.Header.Set("X-CSRF-Token", cookie.Value)
				}
			}
			var response SendChatMessageResponse
			protected := middleware.Auth(testHandler.Queries, nil, nil)(http.HandlerFunc(testHandler.SendChatMessage))
			testutil.Call(test, protected.ServeHTTP, request).Want(http.StatusCreated).JSON(&response)
			var sentVia string
			if err := testPool.QueryRow(context.Background(), `SELECT sent_via FROM chat_message WHERE id = $1`, response.MessageID).Scan(&sentVia); err != nil {
				test.Fatal(err)
			}
			if sentVia != method+"_jwt" {
				test.Fatalf("cookie provenance = %q, want %s_jwt", sentVia, method)
			}
			protected = middleware.Auth(testHandler.Queries, nil, nil)(RequireHumanActor(http.HandlerFunc(testHandler.IssueCliToken)))
			request = request.Clone(request.Context())
			request.URL.Path = "/api/cli-token"
			if method == "cli" {
				testutil.Call(test, protected.ServeHTTP, request).Want(http.StatusForbidden)
			} else {
				var result map[string]string
				testutil.Call(test, protected.ServeHTTP, request).Want(http.StatusOK).JSON(&result)
				if claims := provenanceJWTClaims(test, result["token"]); claims["amr"] != "cli" {
					test.Fatalf("cookie handoff token claims = %v", claims)
				}
			}
		})
	}
}

func TestCachedPATCannotForgeChatProvenance(test *testing.T) {
	redisURL := os.Getenv("REDIS_TEST_URL")
	if redisURL == "" {
		test.Skip("REDIS_TEST_URL not set")
	}
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		test.Fatal(err)
	}
	options.DB = 14
	client := redis.NewClient(options)
	test.Cleanup(func() { client.Close() })
	cache := auth.NewPATCache(client)
	const token = "mul_cached_provenance"
	cache.Set(context.Background(), auth.HashToken(token), testUserID, auth.AuthCacheTTL)
	agentID := createHandlerTestAgent(test, "CachedPATProvenance", []byte("[]"))
	sessionID := createHandlerTestChatSession(test, agentID)
	request := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/messages", map[string]string{
		"content": "cached PAT", "sent_via": "interactive_jwt",
	})
	request = withChatTestWorkspaceCtx(test, withURLParam(request, "sessionId", sessionID))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Actor-Source", "interactive_jwt")
	var response SendChatMessageResponse
	protected := middleware.Auth(nil, cache, nil)(http.HandlerFunc(testHandler.SendChatMessage))
	testutil.Call(test, protected.ServeHTTP, request).Want(http.StatusCreated).JSON(&response)
	var sentVia string
	if err := testPool.QueryRow(context.Background(), `SELECT sent_via FROM chat_message WHERE id = $1`, response.MessageID).Scan(&sentVia); err != nil {
		test.Fatal(err)
	}
	if sentVia != "pat" {
		test.Fatalf("cached PAT provenance = %q, want pat", sentVia)
	}
	request = newRequest(http.MethodPost, "/api/cli-token", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Actor-Source", "interactive_jwt")
	protected = middleware.Auth(nil, cache, nil)(RequireHumanActor(http.HandlerFunc(testHandler.IssueCliToken)))
	testutil.Call(test, protected.ServeHTTP, request).Want(http.StatusForbidden)
}

func TestAutomatedMessageProvenance(test *testing.T) {
	agentID := createHandlerTestAgent(test, "AutomatedProvenance", []byte("[]"))
	sessionID := createHandlerTestChatSession(test, agentID)
	for _, testcase := range []struct {
		name    string
		role    string
		channel bool
		want    string
	}{
		{name: "kickoff", role: "user", want: "system"},
		{name: "assistant", role: "assistant", want: "system"},
		{name: "channel", role: "user", channel: true, want: "channel"},
	} {
		test.Run(testcase.name, func(test *testing.T) {
			message, err := testHandler.Queries.CreateChatMessage(context.Background(), db.CreateChatMessageParams{
				ChatSessionID: parseUUID(sessionID), Role: testcase.role, Content: "automated input",
				ChannelIngested: pgtype.Bool{Bool: testcase.channel, Valid: true},
			})
			if err != nil {
				test.Fatal(err)
			}
			var sentVia string
			if err := testPool.QueryRow(context.Background(), `SELECT sent_via FROM chat_message WHERE id = $1`, message.ID).Scan(&sentVia); err != nil {
				test.Fatal(err)
			}
			if sentVia != testcase.want {
				test.Fatalf("automated provenance = %q, want %q", sentVia, testcase.want)
			}
		})
	}
}
