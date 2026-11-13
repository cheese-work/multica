package handler

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// effectiveDueMaxDepth bounds how far a due date travels: an issue reads dates
// up to this many hops away (parent or done-prerequisite edges).
const effectiveDueMaxDepth = 10

// Graph work bounds, so one request cannot amplify into unbounded CPU, memory or
// DB work: nodes visited per root, cycle members reported per diagnostic, and
// nodes held in memory per call (a var so tests can lower it).
const (
	effectiveDueMaxVisited = 500
	effectiveDueMaxMembers = 20
)

var effectiveDueMaxNodes = 10000

// errEffectiveDueTooBroad means the related graph exceeds effectiveDueMaxNodes.
var errEffectiveDueTooBroad = errors.New("effective_due graph too large")

// DueDiagnostic explains why a projection may be incomplete. Type is "cycle"
// (Members lists up to effectiveDueMaxMembers of the cycle's issue keys),
// "depth_limit" or "graph_limit" (the walk stopped at effectiveDueMaxVisited and no date is published).
type DueDiagnostic struct {
	Type    string   `json:"type"`
	Members []string `json:"members,omitempty"`
}

type dueProjection struct {
	date        string
	source      string
	diagnostics []DueDiagnostic
}

type dueNode struct {
	number  int32
	status  string
	due     pgtype.Date
	parent  pgtype.UUID
	prereqs []pgtype.UUID // open-or-not issues with an enabled other_issue/done wakeup on this node
}

// effectiveDueFor returns, per open issue, the earliest due date it inherits
// from itself, its open parent, or any open issue waiting on it to be done.
// Computed on read: the reachable graph is loaded level by level (one query
// pair per level, at most effectiveDueMaxDepth+1 levels) and walked in memory.
// Closed issues get no entry. Batch-safe: a node's level in the shared frontier
// is its distance to the nearest root, so every node within a root's own depth
// budget is loaded and the walk applies each root's depth limit itself.
func (h *Handler) effectiveDueFor(ctx context.Context, wsID pgtype.UUID, prefix string, roots []db.Issue) (map[pgtype.UUID]dueProjection, error) {
	nodes := map[pgtype.UUID]*dueNode{}
	frontier := make([]pgtype.UUID, 0, len(roots))
	for _, r := range roots {
		nodes[r.ID] = &dueNode{number: r.Number, status: r.Status, due: r.DueDate, parent: r.ParentIssueID}
		frontier = append(frontier, r.ID)
	}
	loaded := map[pgtype.UUID]bool{}
	for depth := 0; depth <= effectiveDueMaxDepth && len(frontier) > 0; depth++ {
		for _, id := range frontier {
			loaded[id] = true
		}
		rows, err := h.DB.Query(ctx, `SELECT (w.condition->>'issue_id')::uuid, w.issue_id FROM issue_wakeup w
			WHERE w.workspace_id=$1 AND w.enabled AND w.disabled_at IS NULL
			AND w.condition->>'type'='other_issue' AND w.condition->>'state'='done'
			AND w.condition->>'issue_id'=ANY($2::text[])`, wsID, uuidStrings(frontier))
		if err != nil {
			return nil, err
		}
		var next []pgtype.UUID
		for rows.Next() {
			var watched, watcher pgtype.UUID
			if err := rows.Scan(&watched, &watcher); err != nil {
				rows.Close()
				return nil, err
			}
			nodes[watched].prereqs = append(nodes[watched].prereqs, watcher)
			next = append(next, watcher)
			if len(nodes)+len(next) > effectiveDueMaxNodes {
				rows.Close()
				return nil, errEffectiveDueTooBroad
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, id := range frontier {
			if p := nodes[id].parent; p.Valid {
				next = append(next, p)
			}
		}
		frontier = frontier[:0]
		for _, id := range next {
			if !loaded[id] && !slices.Contains(frontier, id) {
				frontier = append(frontier, id)
			}
		}
		if len(nodes)+len(frontier) > effectiveDueMaxNodes {
			return nil, errEffectiveDueTooBroad
		}
		if err := h.loadDueNodes(ctx, wsID, nodes, frontier); err != nil {
			return nil, err
		}
	}
	resolver := issuestatus.NewResolver(wsID)
	open := func(id pgtype.UUID) bool {
		n, ok := nodes[id]
		if !ok {
			return false
		}
		c := resolver.Category(ctx, h.Queries, n.status)
		return c != "done" && c != "closed"
	}
	out := map[pgtype.UUID]dueProjection{}
	for _, r := range roots {
		if open(r.ID) {
			out[r.ID] = walkEffectiveDue(prefix, nodes, open, r.ID)
		}
	}
	return out, nil
}

// loadDueNodes fills nodes for ids that exist in the workspace. Missing ids
// (deleted parents or watchers) stay absent and read as closed.
func (h *Handler) loadDueNodes(ctx context.Context, wsID pgtype.UUID, nodes map[pgtype.UUID]*dueNode, ids []pgtype.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := h.DB.Query(ctx, `SELECT id, number, status, due_date, parent_issue_id FROM issue WHERE workspace_id=$1 AND id=ANY($2::uuid[])`, wsID, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id pgtype.UUID
		n := &dueNode{}
		if err := rows.Scan(&id, &n.number, &n.status, &n.due, &n.parent); err != nil {
			return err
		}
		nodes[id] = n
	}
	return rows.Err()
}

func uuidStrings(ids []pgtype.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = uuidToString(id)
	}
	return out
}

type dueStep struct {
	id   pgtype.UUID
	path []pgtype.UUID // root first, id last
	kind []string      // kind[i] is the edge from path[i] to path[i+1]
}

// walkEffectiveDue is a breadth-first walk from root over open issues. A
// neighbor already on the current path is a cycle (the walk stops there); a
// neighbor reached earlier on another path is skipped. Ties on date prefer own,
// then parent, then prereq_of (by the first hop), then the lowest origin key.
func walkEffectiveDue(prefix string, nodes map[pgtype.UUID]*dueNode, open func(pgtype.UUID) bool, root pgtype.UUID) dueProjection {
	key := func(id pgtype.UUID) string { return prefix + "-" + strconv.Itoa(int(nodes[id].number)) }
	rank := map[string]int{"own": 0, "parent": 1, "prereq_of": 2}
	var best *dueStep
	var bestDate string
	var out dueProjection
	type edge struct {
		to   pgtype.UUID
		kind string
	}
	neighbors := func(id pgtype.UUID) []edge {
		n := nodes[id]
		var edges []edge
		if n.parent.Valid && open(n.parent) {
			edges = append(edges, edge{n.parent, "parent"})
		}
		for _, w := range n.prereqs {
			if open(w) {
				edges = append(edges, edge{w, "prereq_of"})
			}
		}
		return edges
	}
	seen := map[pgtype.UUID]bool{root: true}
	depthLimited, visitLimited := false, false
	for queue := []dueStep{{id: root, path: []pgtype.UUID{root}}}; len(queue) > 0; queue = queue[1:] {
		cur := queue[0]
		n := nodes[cur.id]
		if n.due.Valid {
			date := n.due.Time.Format("2006-01-02")
			first := "own"
			if len(cur.kind) > 0 {
				first = cur.kind[0]
			}
			if best == nil || date < bestDate || date == bestDate && (rank[first] < rank[kindOf(best)] ||
				rank[first] == rank[kindOf(best)] && n.number < nodes[best.id].number) {
				best, bestDate = &cur, date
			}
		}
		for _, e := range neighbors(cur.id) {
			if seen[e.to] {
				continue
			}
			if len(cur.path) > effectiveDueMaxDepth {
				depthLimited = true
				continue
			}
			if len(seen) >= effectiveDueMaxVisited {
				visitLimited = true
				continue
			}
			seen[e.to] = true
			queue = append(queue, dueStep{
				id:   e.to,
				path: append(slices.Clone(cur.path), e.to),
				kind: append(slices.Clone(cur.kind), e.kind),
			})
		}
	}
	for _, members := range reachableCycles(seen, func(id pgtype.UUID) []pgtype.UUID {
		var to []pgtype.UUID
		for _, e := range neighbors(id) {
			to = append(to, e.to)
		}
		return to
	}, key) {
		out.diagnostics = append(out.diagnostics, DueDiagnostic{Type: "cycle", Members: members[:min(len(members), effectiveDueMaxMembers)]})
	}
	if depthLimited {
		out.diagnostics = append(out.diagnostics, DueDiagnostic{Type: "depth_limit"})
	}
	if visitLimited {
		out.diagnostics = append(out.diagnostics, DueDiagnostic{Type: "graph_limit"})
	}
	// A walk cut at effectiveDueMaxVisited may have dropped the earliest date, so
	// it publishes none rather than a wrong one; graph_limit tells the caller why.
	if best != nil && !visitLimited {
		out.date = bestDate
		out.source = "own"
		if len(best.kind) > 0 {
			out.source = best.kind[0] + ":" + key(best.id)
			if len(best.path) > 2 {
				via := make([]string, 0, len(best.path)-2)
				for _, id := range best.path[1 : len(best.path)-1] {
					via = append(via, key(id))
				}
				out.source += " via " + strings.Join(via, ">")
			}
		}
	}
	return out
}

// reachableCycles returns one sorted member list per cycle among the reached
// nodes: each strongly connected component of two or more nodes, plus any node
// with an edge to itself (Tarjan). A path-only check misses a cycle whose
// members were all reached first by other paths.
func reachableCycles(reached map[pgtype.UUID]bool, next func(pgtype.UUID) []pgtype.UUID, key func(pgtype.UUID) string) [][]string {
	index, low := map[pgtype.UUID]int{}, map[pgtype.UUID]int{}
	onStack := map[pgtype.UUID]bool{}
	var stack []pgtype.UUID
	var found [][]string
	var visit func(v pgtype.UUID)
	visit = func(v pgtype.UUID) {
		index[v] = len(index) + 1
		low[v] = index[v]
		stack = append(stack, v)
		onStack[v] = true
		selfLoop := false
		for _, w := range next(v) {
			if !reached[w] {
				continue
			}
			selfLoop = selfLoop || w == v
			if index[w] == 0 {
				visit(w)
				low[v] = min(low[v], low[w])
			} else if onStack[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] != index[v] {
			return
		}
		var members []string
		for {
			w := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[w] = false
			members = append(members, key(w))
			if w == v {
				break
			}
		}
		if len(members) > 1 || selfLoop {
			slices.Sort(members)
			found = append(found, members)
		}
	}
	for v := range reached {
		if index[v] == 0 {
			visit(v)
		}
	}
	slices.SortFunc(found, func(a, b []string) int { return strings.Compare(strings.Join(a, ","), strings.Join(b, ",")) })
	return found
}

func kindOf(s *dueStep) string {
	if len(s.kind) == 0 {
		return "own"
	}
	return s.kind[0]
}

// fillEffectiveDue sets the projection fields on resp. A failed read leaves
// them unset: the projection is advisory and must not fail the issue read.
func (h *Handler) fillEffectiveDue(ctx context.Context, issue db.Issue, resp *IssueResponse) {
	prefix := h.getIssuePrefix(ctx, issue.WorkspaceID)
	projections, err := h.effectiveDueFor(ctx, issue.WorkspaceID, prefix, []db.Issue{issue})
	if err != nil {
		return
	}
	if p, ok := projections[issue.ID]; ok {
		if p.date != "" {
			resp.EffectiveDue, resp.DueSource = &p.date, p.source
		}
		resp.Diagnostics = p.diagnostics
	}
}

var ict = time.FixedZone("ICT", 7*3600)

// dueInstant is the instant a YYYY-MM-DD due date expires: the last moment of
// that day in ICT (UTC+7).
func dueInstant(date string) (time.Time, bool) {
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		return time.Time{}, false
	}
	return time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 999999999, ict), true
}

// sortRowsByEffectiveDue orders rows: dated first by expiry instant, then source
// (own, parent, prereq_of), then lowest issue number. Undated rows keep the
// order they arrive in, which the caller sets to priority then oldest.
func sortRowsByEffectiveDue(rows []db.ListIssuesRow, proj map[pgtype.UUID]dueProjection) {
	rank := map[string]int{"own": 0, "parent": 1, "prereq_of": 2}
	type sortKey struct {
		at   time.Time
		rank int
		ok   bool
	}
	keys := make(map[pgtype.UUID]sortKey, len(rows))
	for _, r := range rows {
		p := proj[r.ID]
		at, ok := dueInstant(p.date)
		keys[r.ID] = sortKey{at, rank[strings.SplitN(p.source, ":", 2)[0]], ok}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := keys[rows[i].ID], keys[rows[j].ID]
		switch {
		case a.ok != b.ok:
			return a.ok
		case !a.ok:
			return false
		case !a.at.Equal(b.at):
			return a.at.Before(b.at)
		case a.rank != b.rank:
			return a.rank < b.rank
		}
		return rows[i].Number < rows[j].Number
	})
}
