package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/skillbundle"
)

var errSkillOutsideWorkspace = decisionIntentRefusal{http.StatusNotFound, "skill_not_found: a skill is not in this workspace"}

// skillSetSnapshot holds the digest of the agent's current assignment and of
// the proposed one (every proposed skill enabled), taken under one lock.
type skillSetSnapshot struct {
	LiveSHA256  string
	AfterSHA256 string
	// Live maps each assigned skill id to its enabled flag; Names maps every
	// live and proposed skill id to its name. Both feed the preview.
	Live  map[string]bool
	Names map[string]string
}

// skillSetDigest hashes sorted, deduplicated "skill_id:enabled" pairs and, for
// each of those skills, its bundle manifest hash. Order and duplicates in the
// input do not matter.
func skillSetDigest(enabled map[string]bool, manifests map[string]string) string {
	ids := make([]string, 0, len(enabled))
	for id := range enabled {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	h := sha256.New()
	fmt.Fprint(h, "skill-set/v1\n")
	for _, id := range ids {
		fmt.Fprintf(h, "%s:%t:%s\n", id, enabled[id], manifests[id])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// loadSkillSetSnapshot locks the current and proposed skill rows FOR SHARE
// NOWAIT (a busy row answers busy_retry), then reads the supporting files.
// Every file writer takes a conflicting parent lock first (E4d), so the file
// set stays fixed until the caller's transaction ends. The caller holds the
// agent row lock, which also fixes the assignment.
func loadSkillSetSnapshot(ctx context.Context, queries *db.Queries, workspaceID, agentID pgtype.UUID, proposed []pgtype.UUID) (skillSetSnapshot, error) {
	assigned, err := queries.ListAgentSkillAssignments(ctx, agentID)
	if err != nil {
		return skillSetSnapshot{}, err
	}
	live, after := map[string]bool{}, map[string]bool{}
	wanted := map[string]pgtype.UUID{}
	for _, row := range assigned {
		live[uuidToString(row.SkillID)] = row.Enabled
		wanted[uuidToString(row.SkillID)] = row.SkillID
	}
	for _, id := range proposed {
		after[uuidToString(id)] = true
		wanted[uuidToString(id)] = id
	}
	ids := make([]pgtype.UUID, 0, len(wanted))
	for _, id := range wanted {
		ids = append(ids, id)
	}
	rows, err := queries.LockSkillsShare(ctx, db.LockSkillsShareParams{WorkspaceID: workspaceID, Ids: ids})
	if err != nil {
		if actionTxBusy(err) {
			return skillSetSnapshot{}, errBusyRetry
		}
		return skillSetSnapshot{}, err
	}
	if len(rows) != len(ids) {
		return skillSetSnapshot{}, errSkillOutsideWorkspace
	}
	files, err := queries.ListSkillFilesBySkillIDs(ctx, ids)
	if err != nil {
		return skillSetSnapshot{}, err
	}
	bundles := make(map[string]*skillbundle.Skill, len(rows))
	names := make(map[string]string, len(rows))
	for _, row := range rows {
		id := uuidToString(row.ID)
		names[id] = row.Name
		bundles[id] = &skillbundle.Skill{ID: id, Source: skillbundle.SourceWorkspace, Name: row.Name, Description: row.Description, Content: row.Content}
	}
	for _, file := range files {
		bundle := bundles[uuidToString(file.SkillID)]
		bundle.Files = append(bundle.Files, skillbundle.File{Path: file.Path, Content: file.Content})
	}
	manifests := make(map[string]string, len(bundles))
	for id, bundle := range bundles {
		manifests[id] = skillbundle.BuildManifest(*bundle).Hash
	}
	return skillSetSnapshot{
		LiveSHA256:  skillSetDigest(live, manifests),
		AfterSHA256: skillSetDigest(after, manifests),
		Live:        live,
		Names:       names,
	}, nil
}
