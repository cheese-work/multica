package service

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func provenanceFakeToken() string {
	return strings.Join([]string{"gh", "p_", strings.Repeat("A1b2", 10)}, "")
}

func provUUID(b byte) pgtype.UUID {
	var u pgtype.UUID
	u.Bytes[15] = b
	u.Valid = true
	return u
}

func provTS(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// provTasks is a verifier that accepts exactly the given task ids and counts
// lookups.
func provTasks(calls *int, ok ...pgtype.UUID) ProvenanceTaskVerifier {
	return func(id pgtype.UUID) (bool, error) {
		*calls++
		for _, want := range ok {
			if want == id {
				return true, nil
			}
		}
		return false, nil
	}
}

// provComment builds a comment unchanged since it was created at createdAt.
func provComment(id byte, authorType string, createdAt time.Time) db.Comment {
	return db.Comment{ID: provUUID(id), IssueID: provUUID(9), AuthorType: authorType, Content: "body", CreatedAt: provTS(createdAt), UpdatedAt: provTS(createdAt)}
}

func TestProvenanceExportClassifiesComments(t *testing.T) {
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	var calls int
	e := NewProvenanceExport(cutoff, provTasks(&calls, provUUID(77)))
	rows := []db.Comment{
		{ID: provUUID(1), IssueID: provUUID(9), AuthorType: "member", Content: "at cutoff", CreatedAt: provTS(cutoff), UpdatedAt: provTS(cutoff)},
		{ID: provUUID(2), IssueID: provUUID(9), AuthorType: "member", Content: "late", CreatedAt: provTS(cutoff.Add(time.Second)), UpdatedAt: provTS(cutoff.Add(time.Second))},
		{ID: provUUID(3), IssueID: provUUID(9), AuthorType: "agent", Content: "no task", CreatedAt: provTS(cutoff.Add(-time.Hour)), UpdatedAt: provTS(cutoff.Add(-time.Hour))},
		{ID: provUUID(4), IssueID: provUUID(9), AuthorType: "agent", SourceTaskID: provUUID(77), Content: "key " + provenanceFakeToken(), CreatedAt: provTS(cutoff.Add(-time.Hour)), UpdatedAt: provTS(cutoff.Add(-time.Hour))},
		{ID: provUUID(5), IssueID: provUUID(9), AuthorType: "member", CreatedAt: provTS(cutoff.Add(-time.Hour)), UpdatedAt: provTS(cutoff.Add(-time.Minute)), DeletedAt: provTS(cutoff.Add(-time.Minute))},
	}
	if err := e.AddComments("issue:X-1", rows); err != nil {
		t.Fatal(err)
	}

	recs := e.Records()
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2: %+v", len(recs), recs)
	}
	if recs[0].Redacted || recs[0].Content != "at cutoff" {
		t.Fatalf("plain record = %+v", recs[0])
	}
	if !recs[1].Redacted || strings.Contains(recs[1].Content, provenanceFakeToken()) || !strings.Contains(recs[1].Content, "[REDACTED") {
		t.Fatalf("secret record not redacted: %+v", recs[1])
	}
	if recs[1].SourceTaskID == "" || recs[1].Digest == "" {
		t.Fatalf("agent record missing provenance/digest: %+v", recs[1])
	}

	reasons := map[ProvenanceExclusionReason]int{}
	for _, x := range e.Exclusions() {
		reasons[x.Reason]++
	}
	if reasons[ProvenanceOutOfCutoff] != 1 || reasons[ProvenanceMissingProvenance] != 1 || reasons[ProvenanceDeleted] != 1 {
		t.Fatalf("exclusions = %+v", e.Exclusions())
	}
}

func TestProvenanceExportOverCapIsReported(t *testing.T) {
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	e := NewProvenanceExport(cutoff, nil)
	rows := make([]db.Comment, ProvenanceMaxCommentsPerSource+1)
	for i := range rows {
		var id pgtype.UUID
		id.Bytes[0], id.Bytes[1], id.Valid = byte(i>>8), byte(i), true
		rows[i] = db.Comment{ID: id, AuthorType: "member", CreatedAt: provTS(cutoff.Add(-time.Hour)), UpdatedAt: provTS(cutoff.Add(-time.Hour))}
	}
	if err := e.AddComments("issue:X-1", rows); err != nil {
		t.Fatal(err)
	}
	if got := len(e.Records()); got != ProvenanceMaxCommentsPerSource {
		t.Fatalf("records = %d", got)
	}
	ex := e.Exclusions()
	if len(ex) != 1 || ex[0].Reason != ProvenanceOverCap || ex[0].Count != 1 {
		t.Fatalf("exclusions = %+v", ex)
	}
}

func TestProvenanceManifestIsContentFreeAndStable(t *testing.T) {
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	build := func(order []int) (ProvenanceManifest, string) {
		e := NewProvenanceExport(cutoff, nil)
		all := []db.Comment{
			{ID: provUUID(1), AuthorType: "member", Content: "secret-ish body one", CreatedAt: provTS(cutoff), UpdatedAt: provTS(cutoff)},
			{ID: provUUID(2), AuthorType: "member", Content: "body two", CreatedAt: provTS(cutoff), UpdatedAt: provTS(cutoff)},
		}
		for _, i := range order {
			if err := e.AddComments("thread:t", all[i:i+1]); err != nil {
				t.Fatal(err)
			}
		}
		e.Exclude("issue:Z-1", "", ProvenanceNotFoundOrDenied, 0)
		m, d, err := e.Manifest()
		if err != nil {
			t.Fatal(err)
		}
		return m, d
	}
	m1, d1 := build([]int{0, 1})
	_, d2 := build([]int{1, 0})
	if d1 != d2 {
		t.Fatalf("manifest digest depends on insertion order: %s vs %s", d1, d2)
	}
	if len(m1.Records) != 2 || m1.Records[0].Digest == "" {
		t.Fatalf("manifest = %+v", m1)
	}
}

func TestProvenanceSourceRefHashesMalformedInput(t *testing.T) {
	bad := "not-a-uuid " + provenanceFakeToken()
	got := ProvenanceSourceRef("thread", bad, false)
	if strings.Contains(got, provenanceFakeToken()) || !strings.HasPrefix(got, "thread:sha256:") {
		t.Fatalf("malformed ref echoed raw: %s", got)
	}
	if ProvenanceSourceRef("issue", "11111111-1111-1111-1111-111111111111", true) != "issue:11111111-1111-1111-1111-111111111111" {
		t.Fatal("safe-to-echo ref should be echoed")
	}
}

func TestProvenanceRequestDigestNormalizes(t *testing.T) {
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	a, err := ProvenanceRequestDigest("ws", []string{"B-2", "A-1", "A-1"}, nil, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := ProvenanceRequestDigest("ws", []string{"A-1", "B-2"}, []string{}, cutoff)
	c, _ := ProvenanceRequestDigest("ws", []string{"A-1", "B-2"}, []string{}, cutoff.Add(time.Second))
	if a != b || a == c {
		t.Fatalf("digests a=%s b=%s c=%s", a, b, c)
	}
}

func TestProvenanceExportRecordCapAggregatesPerSource(t *testing.T) {
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	e := NewProvenanceExport(cutoff, nil)
	batches := ProvenanceMaxRecords/ProvenanceMaxCommentsPerSource + 1
	n := 0
	for batch := 0; batch < batches; batch++ {
		rows := make([]db.Comment, ProvenanceMaxCommentsPerSource)
		for i := range rows {
			var id pgtype.UUID
			id.Bytes[0], id.Bytes[1], id.Bytes[2], id.Valid = byte(n>>16), byte(n>>8), byte(n), true
			n++
			rows[i] = db.Comment{ID: id, AuthorType: "member", CreatedAt: provTS(cutoff), UpdatedAt: provTS(cutoff)}
		}
		if err := e.AddComments(fmt.Sprintf("issue:X-%d", batch), rows); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(e.Records()); got != ProvenanceMaxRecords {
		t.Fatalf("records = %d, want cap %d", got, ProvenanceMaxRecords)
	}
	ex := e.Exclusions()
	want := ProvenanceExclusion{Source: fmt.Sprintf("issue:X-%d", batches-1), Reason: ProvenanceOverCap, Count: ProvenanceMaxCommentsPerSource}
	if len(ex) != 1 || ex[0] != want {
		t.Fatalf("exclusions = %+v, want one aggregated %+v", ex, want)
	}
}

func provReasons(e *ProvenanceExport) map[string]ProvenanceExclusionReason {
	out := map[string]ProvenanceExclusionReason{}
	for _, x := range e.Exclusions() {
		out[x.ID] = x.Reason
	}
	return out
}

func TestProvenanceExportAsOfCutoffStateChanges(t *testing.T) {
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	before := cutoff.Add(-time.Hour)

	edited := provComment(1, "member", before)
	edited.Content = "rewritten after cutoff"
	edited.UpdatedAt = provTS(cutoff.Add(time.Minute))

	deletedLater := provComment(2, "member", before)
	deletedLater.Content = "present at cutoff"
	deletedLater.DeletedAt = provTS(cutoff.Add(time.Minute))

	deletedAtCutoff := provComment(3, "member", before)
	deletedAtCutoff.DeletedAt = provTS(cutoff)

	editedAndDeletedLater := provComment(4, "member", before)
	editedAndDeletedLater.UpdatedAt = provTS(cutoff.Add(time.Minute))
	editedAndDeletedLater.DeletedAt = provTS(cutoff.Add(time.Minute))

	e := NewProvenanceExport(cutoff, nil)
	if err := e.AddComments("issue:X-1", []db.Comment{edited, deletedLater, deletedAtCutoff, editedAndDeletedLater}); err != nil {
		t.Fatal(err)
	}

	recs := e.Records()
	if len(recs) != 1 || recs[0].ID != util.UUIDToString(deletedLater.ID) || recs[0].Content != "present at cutoff" {
		t.Fatalf("records = %+v, want only the comment deleted after the cutoff", recs)
	}
	for _, r := range recs {
		if strings.Contains(r.Content, "rewritten") {
			t.Fatalf("post-cutoff edit leaked: %+v", r)
		}
	}
	reasons := provReasons(e)
	want := map[string]ProvenanceExclusionReason{
		util.UUIDToString(edited.ID):                ProvenanceEditedAfterCutoff,
		util.UUIDToString(deletedAtCutoff.ID):       ProvenanceDeleted,
		util.UUIDToString(editedAndDeletedLater.ID): ProvenanceEditedAfterCutoff,
	}
	for id, reason := range want {
		if reasons[id] != reason {
			t.Errorf("exclusion[%s] = %q, want %q (all: %+v)", id, reasons[id], reason, e.Exclusions())
		}
	}
	if _, ok := reasons[util.UUIDToString(deletedLater.ID)]; ok {
		t.Errorf("comment deleted after cutoff was excluded: %+v", e.Exclusions())
	}
}

func TestProvenanceExportIssueEditedAfterCutoff(t *testing.T) {
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	issue := db.Issue{ID: provUUID(1), CreatorType: "member", Title: "renamed later",
		CreatedAt: provTS(cutoff.Add(-time.Hour)), UpdatedAt: provTS(cutoff.Add(time.Second))}
	e := NewProvenanceExport(cutoff, nil)
	included, err := e.AddIssue("issue:X-1", issue)
	if err != nil || !included {
		t.Fatalf("AddIssue = %v, %v; want existed-at-cutoff", included, err)
	}
	if len(e.Records()) != 0 {
		t.Fatalf("edited issue exported: %+v", e.Records())
	}
	if got := provReasons(e)[util.UUIDToString(issue.ID)]; got != ProvenanceEditedAfterCutoff {
		t.Fatalf("reason = %q, want edited_after_cutoff", got)
	}
}

func TestProvenanceExportVerifiesAgentTaskProvenance(t *testing.T) {
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	before := cutoff.Add(-time.Hour)
	good, foreign := provUUID(70), provUUID(71)

	verified := provComment(1, "agent", before)
	verified.SourceTaskID = good
	verifiedAgain := provComment(2, "agent", before)
	verifiedAgain.SourceTaskID = good
	unresolved := provComment(3, "agent", before)
	unresolved.SourceTaskID = foreign
	member := provComment(4, "member", before)
	member.SourceTaskID = foreign

	var calls int
	e := NewProvenanceExport(cutoff, provTasks(&calls, good))
	if err := e.AddComments("issue:X-1", []db.Comment{verified, verifiedAgain, unresolved, member}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("verifier calls = %d, want one per distinct agent task", calls)
	}
	if got := len(e.Records()); got != 3 {
		t.Fatalf("records = %d, want 3: %+v", got, e.Records())
	}
	if got := provReasons(e)[util.UUIDToString(unresolved.ID)]; got != ProvenanceMissingProvenance {
		t.Fatalf("unresolved task reason = %q, want missing_provenance", got)
	}

	errBoom := fmt.Errorf("lookup down")
	failing := NewProvenanceExport(cutoff, func(pgtype.UUID) (bool, error) { return false, errBoom })
	if err := failing.AddComments("issue:X-1", []db.Comment{verified}); err != errBoom {
		t.Fatalf("verifier error = %v, want propagated", err)
	}
}

func TestProvenanceExportAgentIssueProvenance(t *testing.T) {
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	ts := provTS(cutoff.Add(-time.Hour))
	task := provUUID(70)
	agentIssue := func(id byte, originType string, originID pgtype.UUID) db.Issue {
		return db.Issue{ID: provUUID(id), CreatorType: "agent", CreatedAt: ts, UpdatedAt: ts,
			OriginType: pgtype.Text{String: originType, Valid: originType != ""}, OriginID: originID}
	}
	cases := []struct {
		name  string
		issue db.Issue
		want  bool
	}{
		{"agent_create resolving task", agentIssue(1, "agent_create", task), true},
		{"quick_create resolving task", agentIssue(2, "quick_create", task), true},
		{"agent_create unresolved task", agentIssue(3, "agent_create", provUUID(71)), false},
		{"no origin", agentIssue(4, "", pgtype.UUID{}), false},
		{"non-task origin", agentIssue(5, "autopilot", task), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			e := NewProvenanceExport(cutoff, provTasks(&calls, task))
			included, err := e.AddIssue("issue:X-1", tc.issue)
			if err != nil || !included {
				t.Fatalf("AddIssue = %v, %v", included, err)
			}
			if got := len(e.Records()) == 1; got != tc.want {
				t.Fatalf("exported = %v, want %v (exclusions %+v)", got, tc.want, e.Exclusions())
			}
			if !tc.want && provReasons(e)[util.UUIDToString(tc.issue.ID)] != ProvenanceMissingProvenance {
				t.Fatalf("exclusions = %+v, want missing_provenance", e.Exclusions())
			}
		})
	}
}
