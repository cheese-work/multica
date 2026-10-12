package main

import (
	"strings"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/pkg/featureflag"
	"github.com/multica-ai/multica/server/pkg/jev"
)

// newJevClient constructs the TypeSafe System One client (CHE-683),
// mirroring the Composio integration's optional-dependency shape (see the
// composio block in router.go): a paid external dependency that has no
// configured key must not fail server startup.
//
// A blank apiKey returns (nil, nil) — no client, no error. A non-blank key
// constructs a client wired with [featureflags.JevProductionGate] so the
// jev_enabled flag and per-workspace targeting are enforced before any call
// leaves the process, and leaves the pinned default model untouched.
func newJevClient(apiKey string, flags *featureflag.Service) (*jev.Client, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, nil
	}
	return jev.NewClient(jev.Options{
		APIKey: apiKey,
		Gate:   featureflags.JevProductionGate{Flags: flags},
	})
}
