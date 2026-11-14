package main

import (
	"log/slog"
	"os"
	"strings"

	"github.com/multica-ai/multica/server/internal/cibroker"
	"github.com/multica-ai/multica/server/internal/handler"
)

// configureMergeReceiptOIDC turns on PUT /api/github/merge-receipts/ci (CHE-5594).
// It needs both MULTICA_MERGE_RECEIPT_OIDC_POLICY (a CI broker policy file that
// lists the one workflow) and MULTICA_MERGE_RECEIPT_WORKSPACE_ID. Either one
// missing, or a policy that does not load, leaves the route off. It never falls
// back to a looser identity.
func configureMergeReceiptOIDC(h *handler.Handler) {
	path := strings.TrimSpace(os.Getenv("MULTICA_MERGE_RECEIPT_OIDC_POLICY"))
	workspaceID := strings.TrimSpace(os.Getenv("MULTICA_MERGE_RECEIPT_WORKSPACE_ID"))
	if path == "" || workspaceID == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Error("merge receipt OIDC route stays off: policy unreadable", "error", err)
		return
	}
	policy, err := cibroker.LoadPolicy(data)
	if err != nil {
		slog.Error("merge receipt OIDC route stays off: policy invalid", "error", err)
		return
	}
	verifier := cibroker.NewVerifier(&cibroker.JWKSKeys{})
	verifier.Audience = handler.MergeReceiptAudience
	h.MergeReceiptBroker = &cibroker.Broker{Verifier: verifier, Policy: policy}
	h.MergeReceiptWorkspaceID = workspaceID
}
