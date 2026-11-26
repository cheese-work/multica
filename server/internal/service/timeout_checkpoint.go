package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/redact"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// Timeout checkpoint (CHE-9761): when a run hits its time cap the daemon sends
// the facts it can read from the workdir (repos, last tool calls, whether the
// near-limit warning reached the agent) on FailTask. The server validates the
// field, renders one system comment per failed task and, at the retry's claim,
// points the retry at it. The checkpoint is a comment only: it never enters the
// task `error` text, which failure classification and the resume guards read.

const (
	timeoutCheckpointMaxRepos      = 5
	timeoutCheckpointMaxPathLen    = 120
	timeoutCheckpointMaxBranchLen  = 200
	timeoutCheckpointMaxActions    = 8
	timeoutCheckpointMaxActionLen  = 160
	timeoutCheckpointMaxToolLen    = 64
	timeoutCheckpointMaxReasonLen  = 160
	timeoutCheckpointMaxCommentLen = 4096

	// TimeoutCheckpointCoveragePrefix starts the first line of every checkpoint
	// comment; the claim-time lookup and the duplicate guard match on it.
	TimeoutCheckpointCoveragePrefix = "Coverage:"
	coverageDaemonOnly              = "Coverage: daemon facts only"
	coverageAgentRequestedPrefix    = "Coverage: agent checkpoint requested at "
)

// Warning states the daemon may report.
const (
	TimeoutWarningDelivered    = "delivered"
	TimeoutWarningNotDelivered = "not_delivered"
	TimeoutWarningDisabled     = "disabled"
)

// ICT is the display zone for people (workspace default): UTC+7, no DST.
var ictZone = time.FixedZone("ICT", 7*60*60)

type TimeoutCheckpointRepo struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Head   string `json:"head"`
	Ahead  int    `json:"ahead"`
	Dirty  int    `json:"dirty"`
}

type TimeoutCheckpointAction struct {
	Tool  string    `json:"tool"`
	Input string    `json:"input"`
	Time  time.Time `json:"time"`
}

type TimeoutCheckpointWarning struct {
	State  string    `json:"state"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// TimeoutCheckpoint is the validated form of the FailTask `timeout_checkpoint`
// field.
type TimeoutCheckpoint struct {
	Repos          []TimeoutCheckpointRepo   `json:"repos"`
	LastActions    []TimeoutCheckpointAction `json:"last_actions"`
	TotalToolCalls int                       `json:"total_tool_calls"`
	Warning        TimeoutCheckpointWarning  `json:"warning"`
}

var sha40 = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// validBranchRef applies the git check-ref-format rules a branch name must
// satisfy, minus the ones that only matter for creating refs.
func validBranchRef(s string) bool {
	if s == "" || len(s) > timeoutCheckpointMaxBranchLen || s == "@" {
		return false
	}
	if strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".") ||
		strings.HasPrefix(s, "-") || strings.Contains(s, "..") || strings.Contains(s, "@{") ||
		strings.Contains(s, "//") || strings.HasSuffix(s, ".lock") {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return false
		}
	}
	for _, part := range strings.Split(s, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// ParseTimeoutCheckpoint validates the raw field leniently. Invalid entries are
// dropped one by one; a field that is not a JSON object yields nil. It never
// returns an error: a bad checkpoint must not reject the fail report.
func ParseTimeoutCheckpoint(raw json.RawMessage) *TimeoutCheckpoint {
	var top struct {
		Repos          []json.RawMessage `json:"repos"`
		LastActions    []json.RawMessage `json:"last_actions"`
		TotalToolCalls json.RawMessage   `json:"total_tool_calls"`
		Warning        json.RawMessage   `json:"warning"`
	}
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &top) != nil {
		return nil
	}
	cp := &TimeoutCheckpoint{}
	for _, r := range top.Repos {
		if len(cp.Repos) >= timeoutCheckpointMaxRepos {
			break
		}
		var repo TimeoutCheckpointRepo
		if json.Unmarshal(r, &repo) != nil || repo.Path == "" || len(repo.Path) > timeoutCheckpointMaxPathLen ||
			hasControl(repo.Path) || !validBranchRef(repo.Branch) || !sha40.MatchString(repo.Head) ||
			repo.Ahead < 0 || repo.Dirty < 0 {
			continue
		}
		repo.Path = redact.Text(repo.Path)
		repo.Head = strings.ToLower(repo.Head)
		cp.Repos = append(cp.Repos, repo)
	}
	var actions []TimeoutCheckpointAction
	for _, a := range top.LastActions {
		var act TimeoutCheckpointAction
		if json.Unmarshal(a, &act) != nil || act.Tool == "" || len(act.Tool) > timeoutCheckpointMaxToolLen || hasControl(act.Tool) {
			continue
		}
		act.Tool = redact.Text(act.Tool)
		act.Input = oneLine(truncateRunes(redact.Text(act.Input), timeoutCheckpointMaxActionLen))
		actions = append(actions, act)
	}
	if len(actions) > timeoutCheckpointMaxActions {
		actions = actions[len(actions)-timeoutCheckpointMaxActions:]
	}
	cp.LastActions = actions
	var total int
	if json.Unmarshal(top.TotalToolCalls, &total) == nil && total > 0 {
		cp.TotalToolCalls = total
	}
	if cp.TotalToolCalls < len(cp.LastActions) {
		cp.TotalToolCalls = len(cp.LastActions)
	}
	var w TimeoutCheckpointWarning
	if json.Unmarshal(top.Warning, &w) == nil {
		switch w.State {
		case TimeoutWarningDelivered, TimeoutWarningNotDelivered, TimeoutWarningDisabled:
			w.Reason = oneLine(truncateRunes(redact.Text(w.Reason), timeoutCheckpointMaxReasonLen))
			cp.Warning = w
		}
	}
	return cp
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// oneLine keeps a value on one Markdown line and out of code-span breakouts.
func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '`' {
			return '\''
		}
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// RenderTimeoutCheckpoint renders the comment body: capped at 4 KB, redacted,
// with every mention link neutralized so the comment cannot start a run.
func RenderTimeoutCheckpoint(cp TimeoutCheckpoint, attempt int32) string {
	for {
		out := renderTimeoutCheckpointOnce(cp, attempt)
		if len(out) <= timeoutCheckpointMaxCommentLen || len(cp.LastActions) <= 1 {
			if len(out) > timeoutCheckpointMaxCommentLen {
				out = truncateBytes(out, timeoutCheckpointMaxCommentLen)
			}
			return out
		}
		cp.LastActions = cp.LastActions[1:]
	}
}

func truncateBytes(s string, n int) string {
	const marker = "\n(truncated)"
	cut := n - len(marker)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + marker
}

func renderTimeoutCheckpointOnce(cp TimeoutCheckpoint, attempt int32) string {
	var b strings.Builder
	switch {
	case cp.Warning.State == TimeoutWarningDelivered && !cp.Warning.At.IsZero():
		fmt.Fprintf(&b, "%s%s ICT; daemon facts below\n", coverageAgentRequestedPrefix, cp.Warning.At.In(ictZone).Format("15:04"))
	default:
		b.WriteString(coverageDaemonOnly + "\n")
	}
	fmt.Fprintf(&b, "\nAttempt %d hit the run time cap.\n", attempt)
	switch cp.Warning.State {
	case TimeoutWarningNotDelivered:
		b.WriteString("Near-limit warning: not delivered")
		if cp.Warning.Reason != "" {
			b.WriteString(" (" + cp.Warning.Reason + ")")
		}
		b.WriteString("\n")
	case TimeoutWarningDisabled:
		b.WriteString("Near-limit warning: disabled\n")
	}

	b.WriteString("\nRepos:\n")
	if len(cp.Repos) == 0 {
		b.WriteString("- No git repository recorded\n")
	}
	for _, r := range cp.Repos {
		fmt.Fprintf(&b, "- `%s`: branch `%s`, head `%s`, %d unpushed commit(s), %d changed file(s)\n",
			oneLine(r.Path), r.Branch, r.Head, r.Ahead, r.Dirty)
	}

	b.WriteString("\nLast actions:\n")
	if len(cp.LastActions) == 0 {
		b.WriteString("No tool call recorded\n")
	} else {
		for _, a := range cp.LastActions {
			at := ""
			if !a.Time.IsZero() {
				at = a.Time.In(ictZone).Format("15:04:05") + " "
			}
			fmt.Fprintf(&b, "- %s`%s` `%s`\n", at, oneLine(a.Tool), oneLine(a.Input))
		}
		total := cp.TotalToolCalls
		if total < len(cp.LastActions) {
			total = len(cp.LastActions)
		}
		fmt.Fprintf(&b, "(last %d of %d tool calls)\n", len(cp.LastActions), total)
	}
	return strings.ReplaceAll(redact.Text(b.String()), "mention://", "mention-removed://")
}

// TimeoutCheckpointRetryPointer is the claim-time paragraph for a retry whose
// parent failed on its time cap. The form follows the checkpoint's first line:
// only a delivered warning lets the retry rely on the agent's own checkpoint.
func TimeoutCheckpointRetryPointer(attempt int32, commentID, commentContent string) string {
	first, _, _ := strings.Cut(commentContent, "\n")
	if rest, ok := strings.CutPrefix(first, coverageAgentRequestedPrefix); ok {
		at, _, _ := strings.Cut(rest, ";")
		return fmt.Sprintf("Attempt %d hit the run cap. Read checkpoint comment %s, then your own checkpoint comment posted after the warning at %s. "+
			"Check the branch with `git log` and `git status` before you continue. Skip only history your own checkpoint covers.",
			attempt, commentID, strings.TrimSpace(at))
	}
	return fmt.Sprintf("Attempt %d hit the run cap. Checkpoint comment %s has daemon facts only (branch, head, last actions); it does not say what is done. "+
		"Check the branch with `git log` and `git status`, then run the normal thread scan before you continue.",
		attempt, commentID)
}

// IsTimeoutCheckpointReason reports whether a failure reason is one whose retry
// gets a checkpoint pointer.
func IsTimeoutCheckpointReason(reason string) bool {
	return reason == string(taskfailure.ReasonTimeout) || reason == "codex_semantic_inactivity"
}

// normalizeFailureReason mirrors the reason FailTask will persist.
func normalizeFailureReason(errMsg, failureReason string) string {
	errMsg = util.SanitizeTextForPostgres(errMsg)
	if failureReason == "" {
		failureReason = taskfailure.Classify(errMsg).String()
	}
	return taskfailure.NormalizeDaemonReason(failureReason, errMsg).String()
}

// PostTimeoutCheckpoint posts the checkpoint comment for a task that is about
// to fail by timeout, once per task, before the fail transaction mints the
// retry. It is best-effort: every error is logged and swallowed so the fail
// report always lands. It posts nothing for an old daemon (no field), an
// invalid field, a non-timeout reason, a task that cannot fail any more or a
// task with no issue.
func (s *TaskService) PostTimeoutCheckpoint(ctx context.Context, taskID pgtype.UUID, errMsg, failureReason string, raw json.RawMessage) {
	cp := ParseTimeoutCheckpoint(raw)
	if cp == nil || !IsTimeoutCheckpointReason(normalizeFailureReason(errMsg, failureReason)) {
		return
	}
	task, err := s.Queries.GetAgentTask(ctx, taskID)
	if err != nil || !task.IssueID.Valid {
		return
	}
	switch task.Status {
	case "dispatched", "running", "waiting_local_directory":
	default:
		return
	}
	content := RenderTimeoutCheckpoint(*cp, task.Attempt)
	guard := func(qtx *db.Queries) (bool, error) {
		if err := qtx.LockTimeoutCheckpoint(ctx, task.ID); err != nil {
			return false, err
		}
		_, err := qtx.GetTimeoutCheckpointComment(ctx, db.GetTimeoutCheckpointCommentParams{IssueID: task.IssueID, SourceTaskID: task.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return err == nil, err // found: already posted for this task
	}
	if _, err := s.createAgentCommentGuarded(ctx, task.IssueID, task.AgentID, content, "system", task.TriggerCommentID, task.ID, guard); err != nil {
		slog.Warn("timeout checkpoint comment failed", "task_id", util.UUIDToString(task.ID), "error", err)
	}
}
