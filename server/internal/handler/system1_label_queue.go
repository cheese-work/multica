package handler

import (
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// CHE-4383 B4b: the blind label queue. The assigned labeller's task token reads
// the observations it must label. The queue holds ids and typed features only:
// never a prediction (not the observation's, not system1_prediction's) and never
// option_keys, whose stored order puts the asker's recommendation first. The
// sample (ids and seed) is stored when served, so a task that asks again gets
// the same sample and the denominator is reproducible.

const system1QueueCap = 20

// system1QueueFeatures is the typed-feature whitelist per scope. A new stored
// input is never served until it is named here.
var system1QueueFeatures = map[string][]string{"test_first_waiver": {"waiver"}}

type system1QueueItem struct {
	ObservationID string         `json:"observation_id"`
	Scope         string         `json:"scope"`
	Features      map[string]any `json:"features"`
	CreatedAt     string         `json:"created_at"`
}

func (h *Handler) GetSystem1LabelQueue(w http.ResponseWriter, r *http.Request) {
	workspaceID, task, agent, ok := h.system1TaskIdentity(w, r)
	if !ok {
		return
	}
	scope, kind := r.URL.Query().Get("scope"), r.URL.Query().Get("kind")
	if _, known := system1LabelValues[scope]; !known || (kind != "burn_in" && kind != "eval") {
		writeError(w, http.StatusBadRequest, "scope (a known scope) and kind (burn_in or eval) are required")
		return
	}

	ctx := r.Context()
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read label queue")
		return
	}
	defer tx.Rollback(ctx)

	// The caller must be a running task of a live agent, and the assigned labeller for the scope.
	var assigned string
	err = tx.QueryRow(ctx, `SELECT coalesce(w.settings->'system1_labellers'->>$4,'') FROM agent_task_queue t
 JOIN agent a ON a.id=t.agent_id AND a.workspace_id=$3 AND a.archived_at IS NULL
 JOIN workspace w ON w.id=a.workspace_id
 WHERE t.id=$1 AND t.agent_id=$2 AND t.status='running'`, task, agent, workspaceID, scope).Scan(&assigned)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusForbidden, "the queue is read by a running agent task only")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read label queue")
		return
	}
	if assigned == "" || assigned != uuidToString(agent) {
		writeError(w, http.StatusForbidden, "agent is not the assigned labeller for this scope")
		return
	}

	// One sample per (task, scope, kind): serialise, then serve the stored one if it exists.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('system1_queue:'||$1::text||$2||$3, 0))`, uuidToString(task), scope, kind); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read label queue")
		return
	}
	var sampleID pgtype.UUID
	var seed int64
	var ids []pgtype.UUID
	err = tx.QueryRow(ctx, `SELECT id, seed, observation_ids FROM system1_label_sample WHERE task_id=$1 AND scope=$2 AND kind=$3`, task, scope, kind).Scan(&sampleID, &seed, &ids)
	if errors.Is(err, pgx.ErrNoRows) {
		// Eligible: this scope's observations of the workspace with no live labeller row of this kind.
		// The sample is the first 20 by a seeded hash order, so the seed reproduces it.
		seed = rand.Int64()
		rows, qerr := tx.Query(ctx, `SELECT o.id FROM system1_observation o
 WHERE o.workspace_id=$1 AND o.kind='decision' AND o.inputs->>'topic'=$2
 AND NOT EXISTS (SELECT 1 FROM system1_label l WHERE l.observation_id=o.id AND l.kind=$3 AND l.source='labeller' AND l.superseded_at IS NULL)
 ORDER BY md5(o.id::text || $4::bigint::text), o.id LIMIT $5`, workspaceID, scope, kind, seed, system1QueueCap)
		if qerr != nil {
			writeError(w, http.StatusInternalServerError, "failed to read label queue")
			return
		}
		ids, qerr = pgx.CollectRows(rows, pgx.RowTo[pgtype.UUID])
		if qerr != nil {
			writeError(w, http.StatusInternalServerError, "failed to read label queue")
			return
		}
		if len(ids) == 0 {
			// Nothing to label: serve an empty queue and store nothing, so a later ask can still get a sample.
			writeJSON(w, http.StatusOK, map[string]any{"scope": scope, "kind": kind, "items": []system1QueueItem{}})
			return
		}
		if err = tx.QueryRow(ctx, `INSERT INTO system1_label_sample (workspace_id, scope, kind, task_id, agent_id, seed, observation_ids)
 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`, workspaceID, scope, kind, task, agent, seed, ids).Scan(&sampleID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read label queue")
			return
		}
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read label queue")
		return
	}

	// Typed features only, in the stored sample order. An id outside this workspace is not served.
	items := make([]system1QueueItem, 0, len(ids))
	for _, id := range ids {
		var inputs []byte
		var createdAt pgtype.Timestamptz
		err := tx.QueryRow(ctx, `SELECT inputs, created_at FROM system1_observation WHERE id=$1 AND workspace_id=$2`, id, workspaceID).Scan(&inputs, &createdAt)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read label queue")
			return
		}
		var stored map[string]any
		_ = json.Unmarshal(inputs, &stored)
		features := map[string]any{}
		for _, name := range system1QueueFeatures[scope] {
			if v, has := stored[name]; has {
				features[name] = v
			}
		}
		items = append(items, system1QueueItem{ObservationID: uuidToString(id), Scope: scope, Features: features, CreatedAt: createdAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")})
	}
	if err := tx.Commit(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read label queue")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sample_id": uuidToString(sampleID), "scope": scope, "kind": kind, "seed": seed, "items": items})
}
