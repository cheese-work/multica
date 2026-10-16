package daemon

import (
	"path/filepath"
	"strings"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/internal/taskgateway"
	"github.com/multica-ai/multica/server/pkg/credentialexec"
)

func taskGatewayInputs(task Task, provider string) (map[string][]byte, error) {
	if validateTaskIdentity(task) != nil || len(task.Agent.SkillRefs) != 0 || provider != "claude" && provider != "codex" || len(task.Agent.Instructions) > 1<<20 || len(task.WorkspaceContext) > 1<<20 {
		return nil, taskgateway.ErrUnavailable
	}
	prompt := BuildPrompt(task, provider)
	if len(prompt) > 1<<20 {
		return nil, taskgateway.ErrUnavailable
	}
	taskContext := taskContextForEnv(task, provider)
	brief := execenv.BuildRuntimeBrief(provider, taskContext)
	resources, err := execenv.BuildProjectResources(taskContext)
	if err != nil {
		return nil, taskgateway.ErrUnavailable
	}
	inputs := map[string][]byte{
		"multica-input/prompt.md":              []byte(prompt),
		"multica-input/instructions.md":        []byte(task.Agent.Instructions),
		"multica-input/workspace.md":           []byte(task.WorkspaceContext),
		"multica-input/runtime.md":             []byte(brief),
		"multica-input/project/resources.json": resources,
	}
	total := len(prompt) + len(task.Agent.Instructions) + len(task.WorkspaceContext) + len(brief) + len(resources)
	for _, skill := range task.Agent.Skills {
		if skill.Name == "" || skill.Name == "." || skill.Name == ".." || filepath.Base(skill.Name) != skill.Name || strings.ContainsAny(skill.Name, "\\\x00") || len(skill.Content) > 1<<20 || len(inputs) >= 128 || total+len(skill.Content) > 8<<20 {
			return nil, taskgateway.ErrUnavailable
		}
		prefix := "multica-input/skills/" + skill.Name + "/"
		name := prefix + "SKILL.md"
		if _, exists := inputs[name]; exists {
			return nil, taskgateway.ErrUnavailable
		}
		inputs[name] = []byte(skill.Content)
		total += len(skill.Content)
		for _, file := range skill.Files {
			name := prefix + file.Path
			if !filepath.IsLocal(file.Path) || filepath.ToSlash(filepath.Clean(file.Path)) != file.Path || len(file.Content) > 1<<20 || len(inputs) >= 128 || total+len(file.Content) > 8<<20 {
				return nil, taskgateway.ErrUnavailable
			}
			if _, exists := inputs[name]; exists {
				return nil, taskgateway.ErrUnavailable
			}
			inputs[name] = []byte(file.Content)
			total += len(file.Content)
		}
	}
	if credentialexec.ValidateInputs(inputs) != nil {
		return nil, taskgateway.ErrUnavailable
	}
	return inputs, nil
}
