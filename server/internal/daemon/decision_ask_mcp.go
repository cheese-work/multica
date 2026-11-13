package daemon

import (
	"context"
	"encoding/json"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// decision_ask is a built-in tool on the plugin hook MCP server. Its call goes
// to the decision route with the agent's task token, so the MCP process holds no
// credential beyond the one the agent already has. Issue, agent and person come
// from the task on the server; the arguments are only the ask.
const (
	decisionAskToolName = "decision_ask"
	// decisionAskInstallation cannot collide with a plugin: those are UUIDs.
	decisionAskInstallation = "builtin"
	decisionAskDescription  = "Ask the person who requested this issue one decision. The server picks the " +
		"person and the DM lane and parks the issue until they answer. Arguments: ask (context_eli10, " +
		"agent_did, decision, stakes_if_wrong, options[{label, what_happens, pros, cons, cost_or_time}], " +
		"recommendation, recommended_option (exact label; required with two or more options), confidence (optional, 0-1), default_if_no_reply, source_issue_id, source_comment_id), escalation_class " +
		"(reversible, spend, deploy, publish, irreversible or product_direction), optional footer."
	decisionAskSchema = `{"type":"object","required":["ask","escalation_class"],"properties":{"ask":{"type":"object"},"escalation_class":{"type":"string"},"footer":{"type":"string"},"parent_id":{"type":"string"}},"additionalProperties":false}`
)

// decisionAskReachable reports whether the task's runtime can load the MCP
// config that carries decision_ask. Pi fails to start with any MCP config below
// its MCP floor, and decision_ask would put one on every issue task, so the
// tool stays off pi runs that cannot take it; those use `multica agent decision ask`.
func decisionAskReachable(provider, cliVersion string, builtinRuntime bool) bool {
	return provider != "pi" || agent.PiAcceptsMCP(cliVersion, builtinRuntime)
}

// taskHookTools is the task's plugin hook tools plus decision_ask on issue tasks
// whose runtime can load it.
func taskHookTools(task Task, askReachable bool) []PluginHookTool {
	tools := task.PluginHookTools
	if task.IssueID == "" || task.ChatSessionID != "" || !askReachable {
		return tools
	}
	return append(tools[:len(tools):len(tools)], PluginHookTool{
		InstallationID: decisionAskInstallation, HookKey: decisionAskToolName, Name: decisionAskToolName,
		Description: decisionAskDescription, InputSchema: json.RawMessage(decisionAskSchema),
	})
}

// taskHookInvoker routes decision_ask to the decision route and every other
// tool to the plugin invoker.
func taskHookInvoker(task Task, client *Client, plugin pluginHookInvoker) pluginHookInvoker {
	return func(ctx context.Context, taskID, installationID, hookKey string, input json.RawMessage) (json.RawMessage, error) {
		if installationID != decisionAskInstallation {
			return plugin(ctx, taskID, installationID, hookKey, input)
		}
		return client.AskDecision(ctx, task.AuthToken, input)
	}
}
