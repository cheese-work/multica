package execenv

import "strings"

// perTaskBriefIntro heads the per-task part when it is not empty.
const perTaskBriefIntro = "[Per-task runtime brief]\nThese sections belong to the runtime brief above and vary per task.\n\n"

// SplitRuntimeBrief renders the runtime brief in two parts for a run that gets
// a byte-stable system prompt (CHE-6540). stable holds every section that
// depends only on the agent; its bytes are equal for two tasks of one agent,
// so the prompt cache can reuse it. perTask holds the sections that depend on
// the task (requesting user, repositories, project context) and goes into the
// per-turn prompt, after the cached prefix. Nothing is cut: the two parts hold
// the same sections as RuntimeBrief, in a different place.
func SplitRuntimeBrief(provider string, ctx TaskContextForEnv) (stable, perTask string) {
	stableCtx := ctx
	stableCtx.RequestingUserName = ""
	stableCtx.RequestingUserProfileDescription = ""
	stableCtx.Repos = nil
	stableCtx.ProjectID = ""
	stableCtx.ProjectTitle = ""
	stableCtx.ProjectDescription = ""
	stableCtx.ProjectResources = nil
	stable = buildMetaSkillContent(provider, stableCtx)
	if ctx.BackgroundJob != "" {
		return stable, ""
	}

	var b strings.Builder
	writeRequestingUser(&b, ctx)
	if classifyTask(ctx) != kindQuickCreate {
		writeRepositories(&b, ctx)
	}
	writeProjectContext(&b, ctx)
	if b.Len() == 0 {
		return stable, ""
	}
	return stable, perTaskBriefIntro + b.String()
}
