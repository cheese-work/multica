package ciwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ConfigView is the whole ci_watch_config row an admin edits.
type ConfigView struct {
	Repo                    string     `json:"repo"`
	Mode                    string     `json:"mode"`
	Branches                []string   `json:"branches"`
	Events                  []string   `json:"events"`
	WorkflowsInclude        []string   `json:"workflows_include"`
	WorkflowsExclude        []string   `json:"workflows_exclude"`
	P1Mode                  string     `json:"p1_mode"`
	P2Mode                  string     `json:"p2_mode"`
	P3Mode                  string     `json:"p3_mode"`
	P4Mode                  string     `json:"p4_mode"`
	FixOwnerKind            *string    `json:"fix_owner_kind"`
	FixOwnerID              *string    `json:"fix_owner_id"`
	NotifyMemberID          *string    `json:"notify_member_id"`
	MaxRerunsPerOriginalRun int        `json:"max_reruns_per_original_run"`
	MaxNewIssuesPerDay      int        `json:"max_new_issues_per_day"`
	MaxOwnerWakesPerHour    int        `json:"max_owner_wakes_per_hour"`
	UpdatedAt               *time.Time `json:"updated_at"`
}

// Patch is a validated partial update: column name -> new value.
type Patch struct{ vals map[string]any }

// ValidationError names the field an admin got wrong.
type ValidationError struct{ Field, Msg string }

func (e *ValidationError) Error() string { return e.Field + ": " + e.Msg }

// ConfigResult reports what ApplyConfig did.
type ConfigResult struct {
	Config    ConfigView `json:"config"`
	Changed   []string   `json:"changed"`
	Cancelled int        `json:"cancelled"`
}

// WatchEvent is one recent watcher event for a repo.
type WatchEvent struct {
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"`
	Workflow string    `json:"workflow"`
	Branch   string    `json:"branch"`
	RunID    int64     `json:"run_id"`
	Detail   string    `json:"detail"`
	// Failure events only: the trigger, and what the watcher classified and did.
	Event    string `json:"event"`
	Class    string `json:"class"`
	Protocol string `json:"protocol"`
	Action   string `json:"action"`
	State    string `json:"state"`
	Outcome  string `json:"outcome"`
}

type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

type rowsQuerier interface {
	querier
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

var repoPart = regexp.MustCompile(`^[a-z0-9._-]{1,100}$`)

// ParseRepo splits and lowercases "owner/name" (the form stored in the tables).
func ParseRepo(s string) (owner, name string, err error) {
	parts := strings.Split(strings.ToLower(s), "/")
	if len(parts) != 2 || !repoPart.MatchString(parts[0]) || !repoPart.MatchString(parts[1]) ||
		parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
		return "", "", &ValidationError{Field: "repo", Msg: "must be owner/name"}
	}
	return parts[0], parts[1], nil
}

// DefaultConfig is what a repo with no stored row reads as. Keep it equal to the
// column defaults in migration 993 (a test compares them).
func DefaultConfig(repo string) ConfigView {
	return ConfigView{Repo: repo, Mode: "off", Branches: []string{},
		Events:           []string{"push", "schedule", "workflow_run", "workflow_dispatch"},
		WorkflowsInclude: []string{}, WorkflowsExclude: []string{},
		P1Mode: "shadow", P2Mode: "shadow", P3Mode: "shadow", P4Mode: "shadow",
		MaxRerunsPerOriginalRun: 1, MaxNewIssuesPerDay: 5, MaxOwnerWakesPerHour: 6}
}

const (
	maxListItems = 50
	maxItemLen   = 200
	maxCap       = 100000
)

var eventName = regexp.MustCompile(`^[a-z_]{1,40}$`)

type fieldParser func(raw json.RawMessage) (any, error)

func enumField(allowed ...string) fieldParser {
	return func(raw json.RawMessage) (any, error) {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, errors.New("must be a string")
		}
		for _, a := range allowed {
			if s == a {
				return s, nil
			}
		}
		return nil, fmt.Errorf("must be one of %s", strings.Join(allowed, ", "))
	}
}

func listField(check func(string) bool, what string) fieldParser {
	return func(raw json.RawMessage) (any, error) {
		var l []string
		if string(raw) == "null" || json.Unmarshal(raw, &l) != nil {
			return nil, errors.New("must be a list of strings")
		}
		if len(l) > maxListItems {
			return nil, fmt.Errorf("at most %d entries", maxListItems)
		}
		out := make([]string, 0, len(l))
		for _, v := range l {
			if strings.TrimSpace(v) == "" || len(v) > maxItemLen || strings.IndexFunc(v, unicode.IsControl) >= 0 || !check(v) {
				return nil, fmt.Errorf("every entry must be %s, at most %d bytes", what, maxItemLen)
			}
			out = append(out, v)
		}
		return out, nil
	}
}

func uuidField(raw json.RawMessage) (any, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, errors.New("must be a UUID string or null")
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return nil, errors.New("must be a UUID string or null")
	}
	return id.String(), nil
}

func capField(raw json.RawMessage) (any, error) {
	var n int
	if err := json.Unmarshal(raw, &n); err != nil || n < 0 || n > maxCap {
		return nil, fmt.Errorf("must be an integer from 0 to %d", maxCap)
	}
	return n, nil
}

func anyText(string) bool { return true }

// configFields lists every editable column with its SQL cast and parser. The
// keys are the only column names that ever reach the UPDATE statement.
var configFields = map[string]struct {
	cast  string
	parse fieldParser
}{
	"mode":                        {"text", enumField("off", "shadow", "act")},
	"branches":                    {"text[]", listField(anyText, "a branch glob")},
	"events":                      {"text[]", listField(eventName.MatchString, "a lowercase event name")},
	"workflows_include":           {"text[]", listField(anyText, "a workflow path or name")},
	"workflows_exclude":           {"text[]", listField(anyText, "a workflow path or name")},
	"p1_mode":                     {"text", enumField("shadow", "act")},
	"p2_mode":                     {"text", enumField("shadow", "act")},
	"p3_mode":                     {"text", enumField("shadow", "act")},
	"p4_mode":                     {"text", enumField("shadow", "act")},
	"fix_owner_kind":              {"text", nullable(enumField("squad", "agent"))},
	"fix_owner_id":                {"uuid", uuidField},
	"notify_member_id":            {"uuid", uuidField},
	"max_reruns_per_original_run": {"int", capField},
	"max_new_issues_per_day":      {"int", capField},
	"max_owner_wakes_per_hour":    {"int", capField},
}

func nullable(p fieldParser) fieldParser {
	return func(raw json.RawMessage) (any, error) {
		if string(raw) == "null" {
			return nil, nil
		}
		return p(raw)
	}
}

// ParsePatch validates a partial update. Absent fields stay as they are;
// null clears only the three nullable fields. The owner kind and id travel together.
func ParsePatch(raw map[string]json.RawMessage) (Patch, error) {
	if len(raw) == 0 {
		return Patch{}, &ValidationError{Field: "body", Msg: "no fields to change"}
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	p := Patch{vals: map[string]any{}}
	for _, k := range keys {
		f, ok := configFields[k]
		if !ok {
			return Patch{}, &ValidationError{Field: k, Msg: "unknown field"}
		}
		v, err := f.parse(raw[k])
		if err != nil {
			return Patch{}, &ValidationError{Field: k, Msg: err.Error()}
		}
		p.vals[k] = v
	}
	kind, hasKind := p.vals["fix_owner_kind"]
	id, hasID := p.vals["fix_owner_id"]
	switch {
	case hasKind != hasID:
		missing := "fix_owner_id"
		if hasID {
			missing = "fix_owner_kind"
		}
		return Patch{}, &ValidationError{Field: missing, Msg: "send fix_owner_kind and fix_owner_id together"}
	case hasKind && (kind == nil) != (id == nil):
		missing := "fix_owner_id"
		if id != nil {
			missing = "fix_owner_kind"
		}
		return Patch{}, &ValidationError{Field: missing, Msg: "set both fix_owner_kind and fix_owner_id, or neither"}
	}
	return p, nil
}

// Get returns the validated new value of a field in the patch.
func (p Patch) Get(field string) (any, bool) { v, ok := p.vals[field]; return v, ok }

const configCols = `mode, branches, events, workflows_include, workflows_exclude, p1_mode, p2_mode, p3_mode, p4_mode,
	fix_owner_kind, fix_owner_id::text, notify_member_id::text, max_reruns_per_original_run, max_new_issues_per_day,
	max_owner_wakes_per_hour, updated_at`

// readConfig returns the stored row, or the defaults (UpdatedAt nil) when the repo has none.
func readConfig(ctx context.Context, q querier, ws, owner, name, lock string) (ConfigView, error) {
	c := ConfigView{Repo: owner + "/" + name}
	err := q.QueryRow(ctx, `SELECT `+configCols+` FROM ci_watch_config
		WHERE workspace_id=$1::uuid AND repo_owner=$2 AND repo_name=$3`+lock, ws, owner, name).Scan(
		&c.Mode, &c.Branches, &c.Events, &c.WorkflowsInclude, &c.WorkflowsExclude, &c.P1Mode, &c.P2Mode, &c.P3Mode, &c.P4Mode,
		&c.FixOwnerKind, &c.FixOwnerID, &c.NotifyMemberID, &c.MaxRerunsPerOriginalRun, &c.MaxNewIssuesPerDay,
		&c.MaxOwnerWakesPerHour, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultConfig(c.Repo), nil
	}
	return c, err
}

// GetConfig reads one repo's settings without creating a row.
func GetConfig(ctx context.Context, q querier, ws, owner, name string) (ConfigView, error) {
	return readConfig(ctx, q, ws, owner, name, "")
}

// value is the stored value of a column in the same Go types a Patch holds.
func (c ConfigView) value(col string) any {
	str := func(p *string) any {
		if p == nil {
			return nil
		}
		return *p
	}
	switch col {
	case "mode":
		return c.Mode
	case "branches":
		return c.Branches
	case "events":
		return c.Events
	case "workflows_include":
		return c.WorkflowsInclude
	case "workflows_exclude":
		return c.WorkflowsExclude
	case "p1_mode":
		return c.P1Mode
	case "p2_mode":
		return c.P2Mode
	case "p3_mode":
		return c.P3Mode
	case "p4_mode":
		return c.P4Mode
	case "fix_owner_kind":
		return str(c.FixOwnerKind)
	case "fix_owner_id":
		return str(c.FixOwnerID)
	case "notify_member_id":
		return str(c.NotifyMemberID)
	case "max_reruns_per_original_run":
		return c.MaxRerunsPerOriginalRun
	case "max_new_issues_per_day":
		return c.MaxNewIssuesPerDay
	default:
		return c.MaxOwnerWakesPerHour
	}
}

// auditText renders a value for the audit table; nil is SQL NULL.
func auditText(v any) *string {
	var s string
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		s = x
	case int:
		s = strconv.Itoa(x)
	default:
		b, _ := json.Marshal(x)
		s = string(b)
	}
	return &s
}

func sameText(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// ApplyConfig applies p to one repo's settings in one transaction: it writes one
// audit row per changed field and, when the repo goes to Off or Shadow (or a
// protocol goes to Shadow), cancels the scheduled actions that mode would have run.
func ApplyConfig(ctx context.Context, db TxBeginner, ws, owner, name, actorID string, p Patch) (ConfigResult, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return ConfigResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO ci_watch_config (workspace_id, repo_owner, repo_name)
		VALUES ($1::uuid,$2,$3) ON CONFLICT (workspace_id, repo_owner, repo_name) DO NOTHING`, ws, owner, name); err != nil {
		return ConfigResult{}, err
	}
	old, err := readConfig(ctx, tx, ws, owner, name, " FOR UPDATE")
	if err != nil {
		return ConfigResult{}, err
	}

	var changed []string
	for col, v := range p.vals {
		if !sameText(auditText(old.value(col)), auditText(v)) {
			changed = append(changed, col)
		}
	}
	sort.Strings(changed)
	if len(changed) == 0 {
		return ConfigResult{Config: old, Changed: changed}, tx.Commit(ctx)
	}

	sets, args := make([]string, 0, len(changed)+1), []any{ws, owner, name}
	for _, col := range changed {
		args = append(args, p.vals[col])
		sets = append(sets, fmt.Sprintf("%s=$%d::%s", col, len(args), configFields[col].cast))
	}
	if _, err := tx.Exec(ctx, `UPDATE ci_watch_config SET `+strings.Join(sets, ", ")+`, updated_at=now()
		WHERE workspace_id=$1::uuid AND repo_owner=$2 AND repo_name=$3`, args...); err != nil {
		return ConfigResult{}, err
	}
	for _, col := range changed {
		if _, err := tx.Exec(ctx, `INSERT INTO ci_watch_config_audit
			(workspace_id, config_id, actor_type, actor_id, field, old_value, new_value)
			SELECT workspace_id, id, 'member', $4::uuid, $5, $6, $7 FROM ci_watch_config
			WHERE workspace_id=$1::uuid AND repo_owner=$2 AND repo_name=$3`,
			ws, owner, name, actorID, col, auditText(old.value(col)), auditText(p.vals[col])); err != nil {
			return ConfigResult{}, err
		}
	}

	cancelled, err := cancelForModeChange(ctx, tx, ws, owner, name, old, p, changed)
	if err != nil {
		return ConfigResult{}, err
	}
	cur, err := readConfig(ctx, tx, ws, owner, name, "")
	if err != nil {
		return ConfigResult{}, err
	}
	return ConfigResult{Config: cur, Changed: changed, Cancelled: cancelled}, tx.Commit(ctx)
}

// cancelForModeChange cancels scheduled actions a switch to Off or Shadow must
// stop. A repo switch covers every protocol; a protocol switch covers its own.
func cancelForModeChange(ctx context.Context, tx pgx.Tx, ws, owner, name string, old ConfigView, p Patch, changed []string) (int, error) {
	l := &Ledger{now: time.Now}
	f := CancelFilter{WorkspaceID: ws, RepoOwner: owner, RepoName: name}
	isChanged := func(col string) bool {
		i := sort.SearchStrings(changed, col)
		return i < len(changed) && changed[i] == col
	}
	if m, _ := p.vals["mode"].(string); isChanged("mode") && (m == "off" || m == "shadow") {
		return l.cancelScheduled(ctx, tx, f, ReasonOptedOut, false)
	}
	total := 0
	for i := 1; i <= 4; i++ {
		col := fmt.Sprintf("p%d_mode", i)
		if m, _ := p.vals[col].(string); isChanged(col) && m == "shadow" {
			f.Protocol = fmt.Sprintf("P%d", i)
			n, err := l.cancelScheduled(ctx, tx, f, ReasonOptedOut, false)
			if err != nil {
				return 0, err
			}
			total += n
		}
	}
	return total, nil
}

// ListEvents returns the newest watcher events of one repo: failures seen and
// breaker, cap and notification decisions.
func ListEvents(ctx context.Context, q rowsQuerier, ws, owner, name string, limit int) ([]WatchEvent, error) {
	if limit < 1 {
		limit = 1
	}
	rows, err := q.Query(ctx, `SELECT at, kind, workflow, branch, run_id, run_attempt, detail, event FROM (
			SELECT created_at AS at, 'ci_failure' AS kind, workflow_name AS workflow, head_branch AS branch, run_id, run_attempt, conclusion AS detail, event
			FROM ci_failure_event WHERE workspace_id=$1::uuid AND repo_owner=$2 AND repo_name=$3
			UNION ALL
			SELECT created_at, kind, '', '', run_id, 0, detail, ''
			FROM ci_watch_event WHERE workspace_id=$1::uuid AND repo_owner=$2 AND repo_name=$3
		) e ORDER BY at DESC, run_id DESC LIMIT $4`, ws, owner, name, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WatchEvent{}
	attempts := []int32{}
	for rows.Next() {
		var e WatchEvent
		var attempt int32
		if err := rows.Scan(&e.At, &e.Kind, &e.Workflow, &e.Branch, &e.RunID, &attempt, &e.Detail, &e.Event); err != nil {
			return nil, err
		}
		out = append(out, e)
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return out, enrichEvents(ctx, q, ws, owner, name, out, attempts)
}

// enrichEvents adds what the watcher classified (system-1 observation) and did
// (latest ledger action) to each failure event. Two bounded lookups, keyed by
// the page's run ids; events without a match keep empty fields.
func enrichEvents(ctx context.Context, q rowsQuerier, ws, owner, name string, evs []WatchEvent, attempts []int32) error {
	subjects := []pgtype.UUID{}
	runs := []int64{}
	for i, e := range evs {
		if e.Kind != "ci_failure" {
			continue
		}
		subjects = append(subjects, subjectID(Run{RepoOwner: owner, RepoName: name, RunID: e.RunID, RunAttempt: attempts[i]}))
		runs = append(runs, e.RunID)
	}
	if len(runs) == 0 {
		return nil
	}
	type obs struct{ class, protocol, outcome string }
	bySubject := map[pgtype.UUID]obs{}
	orows, err := q.Query(ctx, `SELECT DISTINCT ON (subject_id) subject_id, class, COALESCE(choice_key,''), COALESCE(outcome_label,'')
		FROM system1_observation WHERE workspace_id=$1::uuid AND kind='ci_event' AND subject_id = ANY($2::uuid[])
		ORDER BY subject_id, created_at DESC`, ws, subjects)
	if err != nil {
		return err
	}
	for orows.Next() {
		var id pgtype.UUID
		var o obs
		if err := orows.Scan(&id, &o.class, &o.protocol, &o.outcome); err != nil {
			orows.Close()
			return err
		}
		bySubject[id] = o
	}
	orows.Close()
	if err := orows.Err(); err != nil {
		return err
	}
	type act struct{ protocol, kind, state string }
	byRun := map[int64]act{}
	arows, err := q.Query(ctx, `SELECT DISTINCT ON (original_run_id) original_run_id, protocol, action_kind, state
		FROM ci_watch_action WHERE workspace_id=$1::uuid AND repo_owner=$2 AND repo_name=$3 AND original_run_id = ANY($4::bigint[])
		ORDER BY original_run_id, updated_at DESC`, ws, owner, name, runs)
	if err != nil {
		return err
	}
	for arows.Next() {
		var id int64
		var a act
		if err := arows.Scan(&id, &a.protocol, &a.kind, &a.state); err != nil {
			arows.Close()
			return err
		}
		byRun[id] = a
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return err
	}
	for i := range evs {
		e := &evs[i]
		if e.Kind != "ci_failure" {
			continue
		}
		o := bySubject[subjectID(Run{RepoOwner: owner, RepoName: name, RunID: e.RunID, RunAttempt: attempts[i]})]
		e.Class, e.Protocol, e.Outcome = o.class, o.protocol, o.outcome
		if a, ok := byRun[e.RunID]; ok {
			e.Action, e.State = a.kind, a.state
			if a.protocol != "" {
				e.Protocol = a.protocol
			}
		}
	}
	return nil
}
