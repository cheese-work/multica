package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// The fork auth middleware stamps X-Actor-Source on every human credential
// (interactive_jwt, cli_jwt, pat). requestRepresentsUser must treat those as the
// person themselves; a cloud-node PAT has no human trigger and stays refused.
func TestHumanCredentialsReachOwnChatAndInboxRoutes(test *testing.T) {
	agentID := createHandlerTestAgent(test, "human-credential-access", []byte("[]"))
	credentials := provenanceCredentials(test, agentID)
	verifier := provenanceCloudVerifier(test)

	routes := []struct {
		name    string
		method  string
		path    string
		body    any
		handler http.HandlerFunc
		want    map[string]int
	}{
		{
			name: "list chat sessions", method: http.MethodGet, path: "/api/chat/sessions",
			handler: testHandler.ListChatSessions,
			want: map[string]int{
				"interactive_jwt": http.StatusOK, "cli_jwt": http.StatusOK, "pat": http.StatusOK,
				"cloud_pat": http.StatusForbidden,
			},
		},
		{
			name: "list inbox", method: http.MethodGet, path: "/api/inbox",
			handler: inboxAccessHandler(),
			want: map[string]int{
				"interactive_jwt": http.StatusOK, "cli_jwt": http.StatusOK, "pat": http.StatusOK,
				// The middleware refuses a cloud-node PAT on this path before the handler.
				"cloud_pat": http.StatusUnauthorized,
			},
		},
		{
			name: "create chat session", method: http.MethodPost, path: "/api/chat/sessions",
			body:    map[string]string{"agent_id": agentID, "title": "human credential access"},
			handler: testHandler.CreateChatSession,
			want:    map[string]int{"interactive_jwt": http.StatusCreated, "cloud_pat": http.StatusForbidden},
		},
	}
	for _, route := range routes {
		for caller, want := range route.want {
			test.Run(route.name+"/"+caller, func(test *testing.T) {
				req := withChatTestWorkspaceCtx(test, newRequest(route.method, route.path, route.body))
				req.Header.Set("Authorization", "Bearer "+credentials[caller])
				// A forged source must not matter: the middleware re-derives it.
				req.Header.Set("X-Actor-Source", "interactive_jwt")
				protected := middleware.Auth(testHandler.Queries, nil, verifier, nil)(route.handler)
				testutil.Call(test, protected.ServeHTTP, req).Want(want)
			})
		}
	}
}
