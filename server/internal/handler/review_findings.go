package handler

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ListReviewFindingCategories lists the review findings of the last `days`
// (default 14) and the `limit` (default 10) most frequent categories among them.
// The Saturday retro and the author's pre-check read the top categories
// (CHE-9307). A finding recorded before categories existed counts as
// "uncategorized".
func (h *Handler) ListReviewFindingCategories(w http.ResponseWriter, r *http.Request) {
	wsUUID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace_id")
	if !ok {
		return
	}
	days, limit := intParam(r, "days", 14, 365), intParam(r, "limit", 10, 100)
	rows, err := h.Queries.ListReviewVerdictFindings(r.Context(), db.ListReviewVerdictFindingsParams{
		WorkspaceID: wsUUID,
		CreatedAt:   pgtype.Timestamptz{Time: time.Now().AddDate(0, 0, -days), Valid: true},
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list review findings")
		return
	}
	type entry struct {
		Issue    int32  `json:"issue_number"`
		Verdict  string `json:"verdict"`
		Severity string `json:"severity"`
		Category string `json:"category"`
		Summary  string `json:"summary"`
		At       string `json:"created_at"`
	}
	type top struct {
		Category string `json:"category"`
		Count    int    `json:"count"`
	}
	findings, counts := []entry{}, map[string]int{}
	for _, row := range rows {
		var fs []ReviewFinding
		_ = json.Unmarshal([]byte(row.Findings), &fs)
		for _, f := range fs {
			if f.Category == "" {
				f.Category = "uncategorized"
			}
			counts[f.Category]++
			findings = append(findings, entry{row.IssueNumber, activityVerdict(row.Action), f.Severity, f.Category, f.Summary, timestampToString(row.CreatedAt)})
		}
	}
	tops := make([]top, 0, len(counts))
	for c, n := range counts {
		tops = append(tops, top{c, n})
	}
	sort.Slice(tops, func(i, j int) bool {
		if tops[i].Count != tops[j].Count {
			return tops[i].Count > tops[j].Count
		}
		return tops[i].Category < tops[j].Category
	})
	if len(tops) > limit {
		tops = tops[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": days, "top_categories": tops, "findings": findings})
}

func activityVerdict(action string) string {
	if action == activityPassed {
		return "PASS"
	}
	return "CHANGES"
}

func intParam(r *http.Request, name string, def, max int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || n < 1 {
		return def
	}
	return min(n, max)
}
