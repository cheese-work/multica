package daemon

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"

	"github.com/multica-ai/multica/server/internal/taskgateway"
	"github.com/multica-ai/multica/server/pkg/skillbundle"
)

func (client *Client) resolveTaskGatewaySkills(ctx context.Context, task Task, provider string) (Task, error) {
	if validateTaskIdentity(task) != nil || len(task.Agent.SkillRefs) > 125 {
		return Task{}, taskgateway.ErrUnavailable
	}
	if len(task.Agent.SkillRefs) == 0 {
		return task, nil
	}
	resolved := task
	resolvedAgent := *task.Agent
	resolved.Agent = &resolvedAgent
	resolved.Agent.SkillRefs = nil
	resolved.Agent.Skills = append([]SkillData(nil), task.Agent.Skills...)
	if _, err := taskGatewayInputs(resolved, provider); err != nil {
		return Task{}, taskgateway.ErrUnavailable
	}
	for _, ref := range task.Agent.SkillRefs {
		digest, err := hex.DecodeString(strings.TrimPrefix(ref.Hash, "sha256:"))
		if ref.ID == "" || len(ref.ID) > 1024 || strings.ContainsAny(ref.ID, "\r\n\x00") || ref.Source != skillbundle.SourceWorkspace && ref.Source != skillbundle.SourceBuiltin && ref.Source != skillbundle.SourcePlugin || len(ref.Hash) != 71 || !strings.HasPrefix(ref.Hash, "sha256:") || err != nil || len(digest) != 32 || ref.SizeBytes < 0 || ref.SizeBytes > 8<<20 || ref.FileCount < 0 || ref.FileCount > 124 || len(ref.Files) > 124 {
			return Task{}, taskgateway.ErrUnavailable
		}
	}
	body, err := json.Marshal(struct {
		Skills []SkillRefData `json:"skills"`
	}{task.Agent.SkillRefs})
	if err != nil || len(body) > taskgateway.HandoffLimit {
		return Task{}, taskgateway.ErrUnavailable
	}
	contents, err := client.taskGatewayRequest(ctx, task, "skill-bundles/resolve", body, 8<<20)
	if err != nil {
		return Task{}, taskgateway.ErrUnavailable
	}
	var response struct {
		Bundles []SkillData `json:"bundles"`
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&response) != nil || decoder.Decode(new(any)) != io.EOF || len(response.Bundles) != len(task.Agent.SkillRefs) {
		return Task{}, taskgateway.ErrUnavailable
	}
	for index, bundle := range response.Bundles {
		ref := task.Agent.SkillRefs[index]
		if !validateSkillBundle(ref, bundle) || bundle.SizeBytes != ref.SizeBytes || skillRefFromBundle(bundle).SizeBytes != ref.SizeBytes {
			return Task{}, taskgateway.ErrUnavailable
		}
		resolved.Agent.Skills = append(resolved.Agent.Skills, bundle)
	}
	if _, err := taskGatewayInputs(resolved, provider); err != nil {
		return Task{}, taskgateway.ErrUnavailable
	}
	return resolved, nil
}
