package handler

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/multica-ai/multica/server/internal/service"
)

func compactChatPrompt(content string, workspaceSettings, agentConfig []byte) string {
	if !service.IsCompactCommand(content) {
		return content
	}
	trimmed := strings.TrimSpace(content)
	focus := strings.TrimSpace(strings.TrimPrefix(trimmed, "/compact"))
	criteria := "Write a handoff-format summary of this session's goal, decisions, evidence, open items, exact IDs and URLs, and next action."
	for _, raw := range [][]byte{workspaceSettings, agentConfig} {
		var settings struct {
			CompactPrompt string `json:"compact_prompt"`
		}
		if json.Unmarshal(raw, &settings) == nil && strings.TrimSpace(settings.CompactPrompt) != "" {
			criteria = strings.TrimSpace(settings.CompactPrompt)
		}
	}
	return fmt.Sprintf(`Compact this web DM in place. Use the resumed conversation; read multica chat history if needed. Do not invent facts or include credentials, tokens, or secrets.

Summary requirements:
%s

Save the summary to a UTF-8 file in your working directory. Run multica chat compact --summary-file <path>. The server keeps the same DM (same id, history and URL), stores your summary, and starts your next turn from it. Do not start a replacement conversation yourself or move decision requests. Report that the same DM was compacted.

Optional focus supplied by the human:
%s`, criteria, focus)
}

func validateCompactPromptConfig(value any) error {
	settings, ok := value.(map[string]any)
	if !ok || settings["compact_prompt"] == nil {
		return nil
	}
	prompt, ok := settings["compact_prompt"].(string)
	if !ok || len(prompt) > 16384 {
		return fmt.Errorf("compact_prompt must be a string of at most 16384 bytes, or null to inherit")
	}
	return nil
}
