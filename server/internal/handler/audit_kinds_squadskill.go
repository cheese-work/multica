package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Audit kinds for squads, skills and agent skill/MCP bindings (CHE-2774 L5a).
func init() {
	// avatar_url is not allowlisted: a URL can carry credentials.
	squad := auditKindSpec{
		TargetType: "squad",
		Allow:      []string{"name", "leader_id", "archived"},
		Text:       []string{"description", "instructions"},
	}
	for _, kind := range []string{"squad.create", "squad.update", "squad.delete"} {
		auditKinds[kind] = squad
	}
	// "member" is "<member_type>:<member_id>:<role>", one squad member per row.
	for _, kind := range []string{"squad.member.add", "squad.member.remove", "squad.member.role.update"} {
		auditKinds[kind] = auditKindSpec{TargetType: "squad", Allow: []string{"member"}}
	}
	// config and files are not allowlisted: a config can hold origin URLs and a
	// file body is arbitrary text. file_count is the only file detail stored.
	skill := auditKindSpec{
		TargetType: "skill",
		Allow:      []string{"name", "file_count"},
		Text:       []string{"description", "content"},
	}
	for _, kind := range []string{"skill.create", "skill.update", "skill.delete"} {
		auditKinds[kind] = skill
	}
	auditKinds["agent.skills.update"] = auditKindSpec{TargetType: "agent", Allow: []string{"skill_ids"}}
	auditKinds["agent.runtime_skill.update"] = auditKindSpec{TargetType: "agent", Allow: []string{"disabled_runtime_skills"}}
	auditKinds["agent.mcp_server.add"] = auditKindSpec{TargetType: "agent", Allow: []string{"mcp_server_ids"}}
}

// squadAuditFields snapshots the squad columns an admin can change.
func squadAuditFields(s db.Squad) map[string]any {
	return map[string]any{
		"name":         s.Name,
		"description":  s.Description,
		"instructions": s.Instructions,
		"leader_id":    uuidToString(s.LeaderID),
		"avatar_url":   auditText(s.AvatarUrl),
		"archived":     s.ArchivedAt.Valid,
	}
}

// squadMemberAuditField is the one-member value of the "member" field.
func squadMemberAuditField(memberType, memberID, role string) string {
	return memberType + ":" + memberID + ":" + role
}

// squadMemberRole reads a member's current role; "" when the member is absent.
func squadMemberRole(ctx context.Context, q *db.Queries, squadID pgtype.UUID, memberType string, memberID pgtype.UUID) (string, error) {
	members, err := q.ListSquadMembers(ctx, squadID)
	if err != nil {
		return "", err
	}
	for _, m := range members {
		if m.MemberType == memberType && m.MemberID == memberID {
			return m.Role, nil
		}
	}
	return "", nil
}

// skillAuditFile is a supporting file reduced to what the snapshot needs.
type skillAuditFile struct{ Path, Content string }

func skillAuditFilesFromRows(files []db.SkillFile) []skillAuditFile {
	out := make([]skillAuditFile, len(files))
	for i, f := range files {
		out[i] = skillAuditFile{f.Path, f.Content}
	}
	return out
}

func skillAuditFilesFromResponses(files []SkillFileResponse) []skillAuditFile {
	out := make([]skillAuditFile, len(files))
	for i, f := range files {
		out[i] = skillAuditFile{f.Path, f.Content}
	}
	return out
}

// skillAuditFields snapshots a skill. config and the file set go in as text for
// the diff only: neither is allowlisted, so a change stores the marker.
func skillAuditFields(name, description, content string, config []byte, files []skillAuditFile) map[string]any {
	digests := make([]string, len(files))
	for i, f := range files {
		sum := sha256.Sum256([]byte(f.Content))
		digests[i] = f.Path + ":" + hex.EncodeToString(sum[:])
	}
	sort.Strings(digests)
	return map[string]any{
		"name":        name,
		"description": description,
		"content":     content,
		"config":      string(config),
		"files":       strings.Join(digests, ","),
		"file_count":  len(files),
	}
}

func skillRowAuditFields(s db.Skill, files []db.SkillFile) map[string]any {
	return skillAuditFields(s.Name, s.Description, s.Content, s.Config, skillAuditFilesFromRows(files))
}

// idsAuditField sorts and joins uuids for a set-valued field.
func idsAuditField(ids []string) string {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

// agentSkillsAuditField reads an agent's bound skill ids as the skill_ids value.
func agentSkillsAuditField(ctx context.Context, q *db.Queries, agentID pgtype.UUID) (string, error) {
	skills, err := q.ListAgentSkills(ctx, agentID)
	if err != nil {
		return "", err
	}
	ids := make([]string, len(skills))
	for i, sk := range skills {
		ids[i] = uuidToString(sk.ID)
	}
	return idsAuditField(ids), nil
}

// disabledRuntimeSkillsAuditField lists the switched-off runtime skills as
// "runtime:provider:root:key[:plugin]", sorted.
func disabledRuntimeSkillsAuditField(skills []DisabledRuntimeSkill) string {
	entries := make([]string, len(skills))
	for i, sk := range skills {
		entries[i] = strings.Join([]string{sk.RuntimeID, sk.Provider, sk.Root, sk.Key}, ":")
		if sk.Plugin != "" {
			entries[i] += ":" + sk.Plugin
		}
	}
	return idsAuditField(entries)
}

// agentMcpServersAuditField reads the bound MCP server ids; the config the
// query also returns is dropped here and never reaches the audit row.
func agentMcpServersAuditField(ctx context.Context, q *db.Queries, agentID pgtype.UUID) (string, error) {
	servers, err := q.ListAgentMcpServers(ctx, agentID)
	if err != nil {
		return "", err
	}
	ids := make([]string, len(servers))
	for i, sv := range servers {
		ids[i] = uuidToString(sv.ID)
	}
	return idsAuditField(ids), nil
}
