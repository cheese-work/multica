package browserlease

import (
	"context"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// leaseParams selects the single lease of the env's workspace.
func leaseParams(t *testing.T, e *env) db.GetBrowserLeaseParams {
	t.Helper()
	var p db.GetBrowserLeaseParams
	if err := testPool.QueryRow(context.Background(), `SELECT id, workspace_id FROM browser_lease WHERE workspace_id = $1`, e.ws).Scan(&p.ID, &p.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	return p
}

func instanceEpoch(t *testing.T, e *env, name string) int64 {
	t.Helper()
	var n int64
	if err := testPool.QueryRow(context.Background(), `SELECT epoch FROM browser_instance WHERE workspace_id = $1 AND name = $2`, e.ws, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func instancePhase(t *testing.T, e *env, name string) string {
	t.Helper()
	var s string
	if err := testPool.QueryRow(context.Background(), `SELECT phase FROM browser_instance WHERE workspace_id = $1 AND name = $2`, e.ws, name).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}
