package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// workspace.delegated_writes.set (CHE-4487) lets an agent propose a change to
// the workspace's delegated_governed_writes setting as an action card; the
// owner approves it with a passkey and the server applies it. The params carry
// the whole setting, which the route's validator (buildDelegatedWrites) checks
// as it checks the route's body. One digest covers the setting with its lists
// sorted and deduplicated, so an agent computes it from the set it means.
//
// Authorize is the effective-authority rule: a proposal may not widen what its
// own proposer is delegated to write, and may not drop the human-decision
// requirement. Removing, narrowing and granting another agent are allowed.

const (
	delegatedWritesSetAction = "workspace.delegated_writes.set"
	delegatedWritesField     = "workspace.delegated_writes"
)

type delegatedWritesSetParams struct {
	WorkspaceID string                       `json:"workspace_id"`
	Config      updateDelegatedWritesRequest `json:"delegated_governed_writes"`
	BaseSHA256  string                       `json:"base_sha256"`
	AfterSHA256 string                       `json:"after_sha256"`
}

func init() {
	decisionaction.Register(&decisionaction.Definition{
		Type: delegatedWritesSetAction, Version: 1, Passkey: decisionaction.PasskeyRequired, MinClass: "spend", Target: decisionaction.TargetWorkspace, RequiresPreview: true,
		Template:  decisionaction.Template{Title: "Change delegated governed writes", Summary: "Sets which agents may propose which governed workspace fields."},
		Normalize: normalizeDelegatedWritesSet,
	})
	registerConfigChange(delegatedWritesSetAction, configChange{
		Fields:    []string{delegatedWritesField},
		Authorize: authorizeDelegatedWrites,
		Lock: func(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, params json.RawMessage) error {
			_, err := loadWorkspaceTarget(ctx, queries, workspaceID, params, http.StatusConflict, true)
			return err
		},
		Read: readDelegatedWrites,
		Apply: func(ctx context.Context, execution decisionActionExecution, read configChangeRead) error {
			after, ok := read.After.(DelegatedWritesConfig)
			if !ok {
				return errors.New("delegated writes read carries no setting")
			}
			_, err := storeDelegatedWrites(ctx, execution.Queries, execution.Action.WorkspaceID, execution.Approver.UserID, after)
			return err
		},
		Publish: publishSettingsPatch,
	})
}

func normalizeDelegatedWritesSet(raw json.RawMessage, slots []decisionaction.SecretSlot) (json.RawMessage, []decisionaction.SecretSlot, error) {
	if len(slots) > 0 {
		return nil, nil, &decisionaction.Error{Code: decisionaction.CodeInvalidParams, Message: delegatedWritesSetAction + " takes no secret slots"}
	}
	var params delegatedWritesSetParams
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&params); err != nil {
		return nil, nil, &decisionaction.Error{Code: decisionaction.CodeInvalidParams, Message: fmt.Sprintf("params: %v", err)}
	}
	out, err := json.Marshal(params)
	return out, nil, err
}

// sortedSet is the items sorted, lower-cased when fold is set, and without
// duplicates: the one form the digest and the stored setting take.
func sortedSet(items []string, fold bool) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if fold {
			item = strings.ToLower(item)
		}
		out = append(out, item)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func normalizedDelegatedWrites(cfg DelegatedWritesConfig) DelegatedWritesConfig {
	cfg.AllowedAgents, cfg.AllowedFields = sortedSet(cfg.AllowedAgents, true), sortedSet(cfg.AllowedFields, false)
	return cfg
}

func delegatedWritesDigest(workspaceID string, cfg DelegatedWritesConfig) (string, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return decisionaction.SettingsDigest(workspaceID, map[string]json.RawMessage{delegatedWritesKey: raw})
}

func readDelegatedWrites(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, raw json.RawMessage) (configChangeRead, error) {
	var params delegatedWritesSetParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return configChangeRead{}, settingsInvalid("params must be an object")
	}
	read := configChangeRead{Field: delegatedWritesField, TargetID: uuidToString(workspaceID), HumanBlocksIntact: true}
	workspace, err := loadWorkspaceTarget(ctx, queries, workspaceID, raw, http.StatusBadRequest, false)
	if err != nil {
		return read, err
	}
	after, invalid := buildDelegatedWrites(ctx, queries, workspaceID, params.Config)
	if invalid != "" {
		return read, settingsInvalid("%s", invalid)
	}
	var settings map[string]any
	_ = json.Unmarshal(workspace.Settings, &settings) // settings that are not an object delegate nothing
	before, err := delegatedWritesFromSettings(settings)
	if err != nil {
		return read, err
	}
	before, after = normalizedDelegatedWrites(before), normalizedDelegatedWrites(after)
	if read.LiveSHA256, err = delegatedWritesDigest(read.TargetID, before); err != nil {
		return read, err
	}
	if read.AfterSHA256, err = delegatedWritesDigest(read.TargetID, after); err != nil {
		return read, err
	}
	read.Before, read.After = before, after
	return read, nil
}

// delegatedFieldsOf is what the setting lets agent write.
func delegatedFieldsOf(cfg DelegatedWritesConfig, agent string) []string {
	if !slices.ContainsFunc(cfg.AllowedAgents, func(a string) bool { return strings.EqualFold(a, agent) }) {
		return nil
	}
	return cfg.AllowedFields
}

// authorizeDelegatedWrites runs at request time and again at approval, each
// time against the live setting Read just returned.
func authorizeDelegatedWrites(ctx context.Context, queries *db.Queries, workspaceID, proposer pgtype.UUID, _ json.RawMessage, read configChangeRead) error {
	agent, err := queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: proposer, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) || err == nil && agent.ArchivedAt.Valid {
		return decisionIntentRefusal{http.StatusConflict, "proposer_revoked: the proposing agent is archived or gone; ask again from a live agent"}
	}
	if err != nil {
		return err
	}
	before, beforeOK := read.Before.(DelegatedWritesConfig)
	after, afterOK := read.After.(DelegatedWritesConfig)
	if !beforeOK || !afterOK {
		return errors.New("delegated writes read carries no setting")
	}
	if before.RequiresHumanDecision && !after.RequiresHumanDecision {
		return decisionIntentRefusal{http.StatusForbidden, "requires_human_decision: a proposal may not drop the human-decision requirement"}
	}
	held := delegatedFieldsOf(before, uuidToString(proposer))
	if gained := slices.DeleteFunc(slices.Clone(delegatedFieldsOf(after, uuidToString(proposer))), func(field string) bool { return slices.Contains(held, field) }); len(gained) > 0 {
		return decisionIntentRefusal{http.StatusForbidden, "authority_escalation: the proposal would give its own proposer write authority over " + strings.Join(gained, ", ")}
	}
	return nil
}
