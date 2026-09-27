package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/pkg/featureflag"
	"github.com/multica-ai/multica/server/pkg/jev"
)

// A blank or whitespace-only key must produce no client and no error: Jev is
// a paid optional dependency, and its absence must not fail server startup.
func TestNewJevClientBlankKeyReturnsNilClientNilError(t *testing.T) {
	flags := featureflag.NewService(featureflag.NewStaticProvider())
	for name, key := range map[string]string{"empty": "", "whitespace": "   "} {
		t.Run(name, func(t *testing.T) {
			c, err := newJevClient(key, flags)
			if err != nil {
				t.Fatalf("newJevClient(%q) error = %v, want nil", key, err)
			}
			if c != nil {
				t.Fatalf("newJevClient(%q) client = %v, want nil", key, c)
			}
		})
	}
}

// A real key constructs a client with the pinned default model retained and
// the production gate attached (proven by the gate denying while the flag is
// off).
func TestNewJevClientWithKeyConstructsGatedClient(t *testing.T) {
	t.Setenv(jev.EgressAllowlistEnv, "api.typesafe.ai:443")
	flags := featureflag.NewService(featureflag.NewStaticProvider())
	c, err := newJevClient("test-key", flags)
	if err != nil {
		t.Fatalf("newJevClient: %v", err)
	}
	if c == nil {
		t.Fatal("newJevClient returned a nil client for a non-blank key")
	}
	if c.Model() != jev.DefaultModel {
		t.Errorf("Model() = %q, want %q (pinned default)", c.Model(), jev.DefaultModel)
	}

	// jev_enabled has no rule configured (defaults off), so the gate wired
	// into this client must deny.
	_, err = c.Evaluate(context.Background(), jev.Request{
		State:     "s",
		Questions: map[string]jev.Question{"q": jev.NewNoul("q?", nil)},
	})
	if !errors.Is(err, jev.ErrDisabled) {
		t.Errorf("Evaluate error = %v, want jev.ErrDisabled (gate should deny with the flag off)", err)
	}
}

func TestRouterAllowsEquivalentCurrentAndPreviousJevCredentialKeys(t *testing.T) {
	if testPool == nil {
		t.Skip("router test database is unavailable")
	}
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	t.Setenv("MULTICA_JEV_SECRET_KEY", key)
	t.Setenv("MULTICA_JEV_SECRET_KEY_PREVIOUS", key)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM governance_jev_credential WHERE workspace_id = $1`, testWorkspaceID)
	})

	router := NewRouter(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil)
	request := httptest.NewRequest(http.MethodPut, "/api/workspaces/"+testWorkspaceID+"/jev/credential", strings.NewReader(`{"api_key":"fake-jev-router-key"}`))
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Workspace-ID", testWorkspaceID)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("PUT with equivalent current/previous keys = %d: %s", response.Code, response.Body.String())
	}
}
