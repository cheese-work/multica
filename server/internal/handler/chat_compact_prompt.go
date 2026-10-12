package handler

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
)

func compactChatPrompt(content string, workspaceSettings, agentConfig []byte) string {
	command, ok := engine.ParseControlCommand(content)
	if !ok || command.Kind != engine.ControlCommandCompact {
		return content
	}
	criteria := "Write a handoff-format summary of this session's goal, decisions, evidence, open items, exact IDs and URLs, and next action."
	for _, raw := range [][]byte{workspaceSettings, agentConfig} {
		var settings struct {
			CompactPrompt string `json:"compact_prompt"`
		}
		if json.Unmarshal(raw, &settings) == nil && strings.TrimSpace(settings.CompactPrompt) != "" {
			criteria = strings.TrimSpace(settings.CompactPrompt)
		}
	}
	return fmt.Sprintf(`Continue this web DM in a fresh session. Use the resumed conversation; read multica chat history if needed. Do not invent facts or include credentials, tokens, or secrets.

Summary requirements:
%s

Save the summary to a UTF-8 file in your working directory. Run multica chat continue --summary-file <path>. The server creates a fresh DM with the same agent, title, creator and project, seeds it with your summary, and archives this DM without deleting its transcript. Do not start a replacement conversation yourself or move decision requests. Report the returned URL of the new DM.

Optional focus supplied by the human:
%s`, criteria, command.Body)
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
