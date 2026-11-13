package daemon

import "github.com/multica-ai/multica/server/internal/daemon/initctx"

// buildInitContext reports how the brief and first prompt split into parts.
func buildInitContext(brief, prompt string) *initctx.Report {
	r := initctx.Build(initctx.Input{Brief: brief, Prompt: prompt})
	return &r
}
