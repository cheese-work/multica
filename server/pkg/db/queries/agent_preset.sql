-- name: CreateAgentPreset :one
INSERT INTO agent_preset (workspace_id, agent_id, name, model, thinking_level, service_tier, context_args, is_default, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetAgentPresetByName :one
SELECT * FROM agent_preset
WHERE workspace_id = $1 AND agent_id = $2 AND name = $3;

-- name: ListAgentPresets :many
SELECT * FROM agent_preset
WHERE workspace_id = $1 AND agent_id = $2
ORDER BY name;

-- UpdateAgentPreset writes every settings column; the caller applies the
-- tri-state merge (omitted = unchanged, null = inherit) before calling.
-- name: UpdateAgentPreset :one
UPDATE agent_preset SET
    model = $4,
    thinking_level = $5,
    service_tier = $6,
    context_args = $7,
    is_default = $8,
    updated_at = now()
WHERE workspace_id = $1 AND agent_id = $2 AND name = $3
RETURNING *;

-- name: ClearAgentPresetDefault :exec
UPDATE agent_preset SET is_default = false, updated_at = now()
WHERE workspace_id = $1 AND agent_id = $2 AND is_default;

-- name: DeleteAgentPreset :execrows
DELETE FROM agent_preset
WHERE workspace_id = $1 AND agent_id = $2 AND name = $3;

-- name: DeleteAgentPresetsByAgent :exec
DELETE FROM agent_preset
WHERE workspace_id = $1 AND agent_id = $2;

-- name: SetChatSessionPresetName :one
UPDATE chat_session SET preset_name = $2
WHERE id = $1
RETURNING *;
