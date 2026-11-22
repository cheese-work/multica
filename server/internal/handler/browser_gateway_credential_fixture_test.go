package handler

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/browserlease"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// seedGatewayCredential gives the test workspace an unrevoked credential for gateway. A grant
// needs one (CHE-8883): without it no instance of that gateway is eligible.
func seedGatewayCredential(t *testing.T, gateway string) db.BrowserGatewayCredential {
	t.Helper()
	row, err := db.New(testPool).CreateBrowserGatewayCredential(context.Background(), db.CreateBrowserGatewayCredentialParams{
		WorkspaceID: parseUUID(testWorkspaceID), Gateway: gateway, CredentialHash: browserlease.HashCredential("mbc_test_" + uuid.NewString()),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), "DELETE FROM browser_gateway_credential WHERE workspace_id = $1", testWorkspaceID)
	})
	return row
}
