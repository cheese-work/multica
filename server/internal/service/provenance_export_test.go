package service

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

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

func TestProvenanceExportClassifiesComments(t *testing.T) {
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	e := NewProvenanceExport(cutoff)
	rows := []db.Comment{
		{ID: provUUID(1), IssueID: provUUID(9), AuthorType: "member", Content: "at cutoff", CreatedAt: provTS(cutoff)},
		{ID: provUUID(2), IssueID: provUUID(9), AuthorType: "member", Content: "late", CreatedAt: provTS(cutoff.Add(time.Second))},
		{ID: provUUID(3), IssueID: provUUID(9), AuthorType: "agent", Content: "no task", CreatedAt: provTS(cutoff.Add(-time.Hour))},
		{ID: provUUID(4), IssueID: provUUID(9), AuthorType: "agent", SourceTaskID: provUUID(77), Content: "key " + provenanceFakeToken(), CreatedAt: provTS(cutoff.Add(-time.Hour))},
		{ID: provUUID(5), IssueID: provUUID(9), AuthorType: "member", CreatedAt: provTS(cutoff.Add(-time.Hour)), DeletedAt: provTS(cutoff.Add(-time.Minute))},
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
	e := NewProvenanceExport(cutoff)
	rows := make([]db.Comment, ProvenanceMaxCommentsPerSource+1)
	for i := range rows {
		var id pgtype.UUID
		id.Bytes[0], id.Bytes[1], id.Valid = byte(i>>8), byte(i), true
		rows[i] = db.Comment{ID: id, AuthorType: "member", CreatedAt: provTS(cutoff.Add(-time.Hour))}
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
		e := NewProvenanceExport(cutoff)
		all := []db.Comment{
			{ID: provUUID(1), AuthorType: "member", Content: "secret-ish body one", CreatedAt: provTS(cutoff)},
			{ID: provUUID(2), AuthorType: "member", Content: "body two", CreatedAt: provTS(cutoff)},
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
	if ProvenanceSourceRef("issue", "MUL-1", true) != "issue:MUL-1" {
		t.Fatal("well-formed ref should be echoed")
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
	e := NewProvenanceExport(cutoff)
	batches := ProvenanceMaxRecords/ProvenanceMaxCommentsPerSource + 1
	n := 0
	for batch := 0; batch < batches; batch++ {
		rows := make([]db.Comment, ProvenanceMaxCommentsPerSource)
		for i := range rows {
			var id pgtype.UUID
			id.Bytes[0], id.Bytes[1], id.Bytes[2], id.Valid = byte(n>>16), byte(n>>8), byte(n), true
			n++
			rows[i] = db.Comment{ID: id, AuthorType: "member", CreatedAt: provTS(cutoff)}
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
