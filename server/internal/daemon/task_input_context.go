package daemon

import (
	"strings"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

func taskContextForEnv(task Task, provider string) execenv.TaskContextForEnv {
	return execenv.TaskContextForEnv{
		IssueID: task.IssueID, TriggerCommentID: task.TriggerCommentID,
		TriggerThreadID: task.TriggerThreadID, CommentReplyTargets: commentReplyThreads(task),
		NewCommentCount: task.NewCommentCount, NewCommentsSince: task.NewCommentsSince,
		PriorSessionResumed: task.PriorSessionID != "", PriorSessionResumeUnavailable: task.PriorSessionResumeUnavailable,
		AgentID: task.AgentID, AgentName: task.Agent.Name, AgentInstructions: task.Agent.Instructions,
		AgentSkills:           convertSkillsForEnv(task.Agent.Skills),
		DisabledRuntimeSkills: convertDisabledRuntimeSkillsForEnv(task.Agent, task.RuntimeID, provider),
		Repos:                 convertReposForEnv(task.Repos), ProjectID: task.ProjectID,
		ProjectTitle: task.ProjectTitle, ProjectDescription: task.ProjectDescription,
		ProjectResources: convertProjectResourcesForEnv(task.ProjectResources),
		ChatSessionID:    task.ChatSessionID, ChatChannelType: task.ChatChannelType,
		ChatChannelDeliversFiles: task.ChatChannelDeliversFiles, AutopilotRunID: task.AutopilotRunID,
		AutopilotID: task.AutopilotID, AutopilotTitle: task.AutopilotTitle,
		AutopilotDescription: task.AutopilotDescription, AutopilotSource: task.AutopilotSource,
		AutopilotTriggerPayload: strings.TrimSpace(string(task.AutopilotTriggerPayload)),
		QuickCreatePrompt:       task.QuickCreatePrompt, IsSquadLeader: taskIsSquadLeader(task),
		RequestingUserName: task.RequestingUserName, RequestingUserProfileDescription: task.RequestingUserProfileDescription,
		InitiatorType: task.InitiatorType, InitiatorID: task.InitiatorID,
		InitiatorName: task.InitiatorName, InitiatorEmail: task.InitiatorEmail,
		WorkspaceContext: task.WorkspaceContext, IssueStatuses: convertIssueStatusesForEnv(task.IssueStatuses),
		IssueStatusesOmitted: task.IssueStatusesOmitted, ConnectedApps: task.ConnectedApps,
	}
}
