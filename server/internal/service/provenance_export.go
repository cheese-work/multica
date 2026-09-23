package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/redact"
)

// CHE-755 provenance export bounds. Every limit that fires is reported as an
// over_cap exclusion; nothing is ever silently truncated.
const (
	ProvenanceMaxSources           = 256
	ProvenanceMaxCommentsPerSource = 500
	ProvenanceMaxRecords           = 5000
	ProvenanceMaxRefLength         = 128
	ProvenanceRedactionPolicy      = "redact.Text:v1"
)

type ProvenanceExclusionReason string

const (
	ProvenanceNotFoundOrDenied    ProvenanceExclusionReason = "not_found_or_denied"
	ProvenanceMalformed           ProvenanceExclusionReason = "malformed"
	ProvenanceOutOfCutoff         ProvenanceExclusionReason = "out_of_cutoff"
	ProvenanceMissingProvenance   ProvenanceExclusionReason = "missing_provenance"
	ProvenanceOverCap             ProvenanceExclusionReason = "over_cap"
	ProvenanceDeleted             ProvenanceExclusionReason = "deleted"
	ProvenanceModifiedAfterCutoff ProvenanceExclusionReason = "modified_after_cutoff"
)

type ProvenanceRecordKind string

const (
	ProvenanceIssueRecord   ProvenanceRecordKind = "issue"
	ProvenanceCommentRecord ProvenanceRecordKind = "comment"
)

type ProvenanceRecord struct {
	Kind         ProvenanceRecordKind `json:"kind"`
	ID           string               `json:"id"`
	IssueID      string               `json:"issue_id"`
	Source       string               `json:"source"`
	ParentID     string               `json:"parent_id,omitempty"`
	AuthorType   string               `json:"author_type"`
	AuthorID     string               `json:"author_id"`
	SourceTaskID string               `json:"source_task_id,omitempty"`
	OriginType   string               `json:"origin_type,omitempty"`
	Title        string               `json:"title,omitempty"`
	Description  string               `json:"description,omitempty"`
	Content      string               `json:"content,omitempty"`
	CreatedAt    string               `json:"created_at"`
	UpdatedAt    string               `json:"updated_at"`
	Revision     int64                `json:"revision"`
	Redacted     bool                 `json:"redacted"`
	Digest       string               `json:"digest"`
}

type ProvenanceExclusion struct {
	Source string                    `json:"source"`
	ID     string                    `json:"id,omitempty"`
	Reason ProvenanceExclusionReason `json:"reason"`
	Count  int                       `json:"count,omitempty"`
	// CountIsFloor marks Count as a lower bound: at least Count rows were
	// dropped, the true number may be larger.
	CountIsFloor bool `json:"count_is_floor,omitempty"`
}

// ProvenanceManifestEntry is the retention projection of a record: ids,
// revision and digest only, never the exported text.
type ProvenanceManifestEntry struct {
	Kind     ProvenanceRecordKind `json:"kind"`
	ID       string               `json:"id"`
	IssueID  string               `json:"issue_id"`
	Revision int64                `json:"revision"`
	Redacted bool                 `json:"redacted"`
	Digest   string               `json:"digest"`
}

type ProvenanceManifest struct {
	Records    []ProvenanceManifestEntry `json:"records"`
	Exclusions []ProvenanceExclusion     `json:"exclusions"`
}

// ProvenanceSourceRef renders a caller-supplied ref for responses and the
// audit manifest. Only a ref whose exact bytes are known to be harmless (a
// parsed UUID) may be echoed; anything else is arbitrary input that could be
// a pasted secret, so only its hash is ever echoed back or stored.
func ProvenanceSourceRef(kind, ref string, safeToEcho bool) string {
	if !safeToEcho {
		sum := sha256.Sum256([]byte(ref))
		return kind + ":sha256:" + hex.EncodeToString(sum[:])
	}
	return kind + ":" + ref
}

// ProvenanceRequestDigest fingerprints the normalized request so two exports
// asking for the same sources at the same cutoff are recognizably identical in
// the audit log regardless of flag order or duplicates.
func ProvenanceRequestDigest(workspaceID string, issues, threads []string, cutoff time.Time) (string, error) {
	return canonicalDigest(struct {
		WorkspaceID string   `json:"workspace_id"`
		Issues      []string `json:"issues"`
		Threads     []string `json:"threads"`
		Cutoff      string   `json:"cutoff"`
	}{workspaceID, sortedUnique(issues), sortedUnique(threads), cutoff.UTC().Format(time.RFC3339Nano)})
}

// ProvenanceTaskVerifier reports whether taskID names a task in the export's
// workspace. It returns false, nil for a task that does not exist or belongs
// to another workspace.
type ProvenanceTaskVerifier func(taskID pgtype.UUID) (bool, error)

// ProvenanceExport accumulates one bounded export. It is not safe for
// concurrent use.
type ProvenanceExport struct {
	cutoff     time.Time
	verifyTask ProvenanceTaskVerifier
	// tasks caches verifyTask results so a task cited by many rows costs one
	// lookup per export.
	tasks      map[[16]byte]bool
	records    []ProvenanceRecord
	exclusions []ProvenanceExclusion
	seen       map[string]bool
	// capped counts rows dropped per source by the export-wide record cap,
	// reported as one over_cap exclusion per source rather than one per row
	// so the audit manifest stays bounded too.
	capped map[string]int
}

// NewProvenanceExport starts an export of rows created at or before cutoff
// and unmodified since. A nil verifyTask treats every agent-authored row as
// lacking provenance.
func NewProvenanceExport(cutoff time.Time, verifyTask ProvenanceTaskVerifier) *ProvenanceExport {
	return &ProvenanceExport{
		cutoff:     cutoff,
		verifyTask: verifyTask,
		tasks:      map[[16]byte]bool{},
		seen:       map[string]bool{},
		capped:     map[string]int{},
	}
}

func (e *ProvenanceExport) Cutoff() time.Time { return e.cutoff }

func (e *ProvenanceExport) Exclude(source, id string, reason ProvenanceExclusionReason, count int) {
	e.exclusions = append(e.exclusions, ProvenanceExclusion{Source: source, ID: id, Reason: reason, Count: count})
}

func (e *ProvenanceExport) afterCutoff(ts time.Time) bool {
	return ts.After(e.cutoff)
}

// changedAfterCutoff reports whether a row's stored state may differ from the
// state it had at the cutoff. There is no edit history to rebuild the earlier
// state from, so such a row is excluded, not reconstructed.
func (e *ProvenanceExport) changedAfterCutoff(updatedAt pgtype.Timestamptz) bool {
	return !updatedAt.Valid || e.afterCutoff(updatedAt.Time)
}

func (e *ProvenanceExport) taskResolves(taskID pgtype.UUID) (bool, error) {
	if !taskID.Valid || e.verifyTask == nil {
		return false, nil
	}
	if ok, cached := e.tasks[taskID.Bytes]; cached {
		return ok, nil
	}
	ok, err := e.verifyTask(taskID)
	if err != nil {
		return false, err
	}
	e.tasks[taskID.Bytes] = ok
	return ok, nil
}

// issueOriginTask returns the agent_task_queue row an agent-created issue is
// stamped with. Only these origin types carry a task id in origin_id; other
// origins (autopilot, chat integrations) name non-task rows.
func issueOriginTask(issue db.Issue) pgtype.UUID {
	switch issue.OriginType.String {
	case "agent_create", "quick_create":
		return issue.OriginID
	}
	return pgtype.UUID{}
}

// AddIssue adds the issue row itself. It reports false when the issue did not
// exist at the cutoff, in which case the caller must not export its comments.
// An issue that existed but is excluded for another reason still reports true:
// its comments are separate rows and are classified on their own.
func (e *ProvenanceExport) AddIssue(source string, issue db.Issue) (bool, error) {
	id := util.UUIDToString(issue.ID)
	if !issue.CreatedAt.Valid || e.afterCutoff(issue.CreatedAt.Time) {
		e.Exclude(source, id, ProvenanceOutOfCutoff, 0)
		return false, nil
	}
	// Activity such as a comment delete bumps revision and last_activity_at
	// without touching updated_at. NULL is not a failure: rows predating the
	// column keep it NULL until their first activity write, which always
	// stamps it, so updated_at alone covers them.
	if e.changedAfterCutoff(issue.UpdatedAt) ||
		(issue.LastActivityAt.Valid && e.afterCutoff(issue.LastActivityAt.Time)) {
		e.Exclude(source, id, ProvenanceModifiedAfterCutoff, 0)
		return true, nil
	}
	var sourceTaskID string
	if issue.CreatorType == "agent" {
		task := issueOriginTask(issue)
		ok, err := e.taskResolves(task)
		if err != nil {
			return false, err
		}
		if !ok {
			e.Exclude(source, id, ProvenanceMissingProvenance, 0)
			return true, nil
		}
		sourceTaskID = util.UUIDToString(task)
	}
	title := redact.Text(issue.Title)
	description := redact.Text(issue.Description.String)
	rec := ProvenanceRecord{
		Kind:         ProvenanceIssueRecord,
		ID:           id,
		IssueID:      id,
		Source:       source,
		AuthorType:   issue.CreatorType,
		AuthorID:     util.UUIDToString(issue.CreatorID),
		OriginType:   issue.OriginType.String,
		SourceTaskID: sourceTaskID,
		Title:        title,
		Description:  description,
		CreatedAt:    provenanceTime(issue.CreatedAt.Time),
		UpdatedAt:    provenanceTime(issue.UpdatedAt.Time),
		Revision:     issue.Revision,
		Redacted:     title != issue.Title || description != issue.Description.String,
	}
	return true, e.add(source, rec)
}

// AddComments classifies rows fetched with a limit of
// ProvenanceMaxCommentsPerSource+1. The extra row proves overflow but cannot
// say how far past the cap the source goes, so the over_cap count is reported
// as a floor rather than paying for a second COUNT query per source.
func (e *ProvenanceExport) AddComments(source string, rows []db.Comment) error {
	if len(rows) > ProvenanceMaxCommentsPerSource {
		e.exclusions = append(e.exclusions, ProvenanceExclusion{
			Source: source, Reason: ProvenanceOverCap,
			Count: len(rows) - ProvenanceMaxCommentsPerSource, CountIsFloor: true,
		})
		rows = rows[:ProvenanceMaxCommentsPerSource]
	}
	for _, c := range rows {
		id := util.UUIDToString(c.ID)
		switch {
		case !c.CreatedAt.Valid || e.afterCutoff(c.CreatedAt.Time):
			e.Exclude(source, id, ProvenanceOutOfCutoff, 0)
			continue
		case c.DeletedAt.Valid && !e.afterCutoff(c.DeletedAt.Time):
			e.Exclude(source, id, ProvenanceDeleted, 0)
			continue
		case e.changedAfterCutoff(c.UpdatedAt):
			e.Exclude(source, id, ProvenanceModifiedAfterCutoff, 0)
			continue
		}
		if c.AuthorType == "agent" {
			ok, err := e.taskResolves(c.SourceTaskID)
			if err != nil {
				return err
			}
			if !ok {
				e.Exclude(source, id, ProvenanceMissingProvenance, 0)
				continue
			}
		}
		content := redact.Text(c.Content)
		rec := ProvenanceRecord{
			Kind:       ProvenanceCommentRecord,
			ID:         id,
			IssueID:    util.UUIDToString(c.IssueID),
			Source:     source,
			AuthorType: c.AuthorType,
			AuthorID:   util.UUIDToString(c.AuthorID),
			Content:    content,
			CreatedAt:  provenanceTime(c.CreatedAt.Time),
			UpdatedAt:  provenanceTime(c.UpdatedAt.Time),
			Revision:   c.Revision,
			Redacted:   content != c.Content,
		}
		if c.ParentID.Valid {
			rec.ParentID = util.UUIDToString(c.ParentID)
		}
		if c.SourceTaskID.Valid {
			rec.SourceTaskID = util.UUIDToString(c.SourceTaskID)
		}
		if err := e.add(source, rec); err != nil {
			return err
		}
	}
	return nil
}

func (e *ProvenanceExport) add(source string, rec ProvenanceRecord) error {
	key := string(rec.Kind) + ":" + rec.ID
	// The same row reached through two sources (an issue and one of its
	// threads) is exported once; the first source to reach it owns it.
	if e.seen[key] {
		return nil
	}
	if len(e.records) >= ProvenanceMaxRecords {
		e.capped[source]++
		return nil
	}
	digest, err := recordDigest(rec)
	if err != nil {
		return err
	}
	rec.Digest = digest
	e.seen[key] = true
	e.records = append(e.records, rec)
	return nil
}

func (e *ProvenanceExport) Records() []ProvenanceRecord {
	return append([]ProvenanceRecord{}, e.records...)
}

func (e *ProvenanceExport) Exclusions() []ProvenanceExclusion {
	out := append([]ProvenanceExclusion{}, e.exclusions...)
	sources := make([]string, 0, len(e.capped))
	for source := range e.capped {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	for _, source := range sources {
		out = append(out, ProvenanceExclusion{Source: source, Reason: ProvenanceOverCap, Count: e.capped[source]})
	}
	return out
}

// Manifest returns the retention manifest (sorted, content-free) and its
// digest.
func (e *ProvenanceExport) Manifest() (ProvenanceManifest, string, error) {
	m := ProvenanceManifest{
		Records:    make([]ProvenanceManifestEntry, 0, len(e.records)),
		Exclusions: e.Exclusions(),
	}
	for _, r := range e.records {
		m.Records = append(m.Records, ProvenanceManifestEntry{
			Kind: r.Kind, ID: r.ID, IssueID: r.IssueID, Revision: r.Revision, Redacted: r.Redacted, Digest: r.Digest,
		})
	}
	sort.Slice(m.Records, func(i, j int) bool {
		a, b := m.Records[i], m.Records[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.ID < b.ID
	})
	sort.Slice(m.Exclusions, func(i, j int) bool {
		a, b := m.Exclusions[i], m.Exclusions[j]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Reason < b.Reason
	})
	digest, err := canonicalDigest(m)
	if err != nil {
		return ProvenanceManifest{}, "", err
	}
	return m, digest, nil
}

// recordDigest covers the exported content, not where it was reached from, so
// the same row digests identically whichever source selected it.
func recordDigest(rec ProvenanceRecord) (string, error) {
	rec.Source = ""
	rec.Digest = ""
	return canonicalDigest(rec)
}

func canonicalDigest(v any) (string, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal provenance digest input: %w", err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func provenanceTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func sortedUnique(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
