package designguard

import "testing"

func TestCheckVerdictTarget(t *testing.T) {
	const origin = "https://doop.example.com"
	const issue = "11111111-1111-1111-1111-111111111111"
	good := Attestation{CanvasID: "c1", PageID: "p1", PageIssueID: issue, Complete: true, Current: true, Frames: []string{"f1", "f2"}}
	with := func(f func(*Attestation)) Attestation { a := good; f(&a); return a }
	for _, tc := range []struct {
		name, url string
		a         Attestation
		want      string
	}{
		{"all hold, page and frame", origin + "/c/c1?page=p1&frame=f1", good, ""},
		{"all hold, page only", origin + "/c/c1?page=p1", good, ""},
		{"all hold, frame only", origin + "/c/c1/proto?frame=f2", good, ""},
		{"C2 empty", "", good, CodeDesignURLMissing},
		{"C2 canvas link without page or frame", origin + "/c/c1", good, CodeDesignURLMissing},
		{"C2 ambiguous", origin + "/c/c1?frame=f1&frame=f2", good, CodeDesignURLAmbiguous},
		{"C3 canvas", origin + "/c/c2?page=p1", good, CodeCanvasMismatch},
		{"C4 page", origin + "/c/c1?page=p2", good, CodeFrameOutsidePage},
		{"C4 frame", origin + "/c/c1?page=p1&frame=f9", good, CodeFrameOutsidePage},
		{"C4b bound elsewhere", origin + "/c/c1?page=p1", with(func(a *Attestation) { a.PageIssueID = "other" }), CodePageNotBound},
		{"C4b bound to nothing", origin + "/c/c1?page=p1", with(func(a *Attestation) { a.PageIssueID = "" }), CodePageNotBound},
		{"C5 incomplete", origin + "/c/c1?page=p1", with(func(a *Attestation) { a.Complete = false }), CodeAttestationIncomplete},
		{"C5 stale", origin + "/c/c1?page=p1", with(func(a *Attestation) { a.Current = false }), CodeAttestationStale},
		{"order: C3 before C5", origin + "/c/c2?page=p1", with(func(a *Attestation) { a.Current = false }), CodeCanvasMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckVerdictTarget(origin, tc.url, issue, tc.a)
			switch {
			case tc.want == "" && got != nil:
				t.Fatalf("refused: %v", got)
			case tc.want != "" && (got == nil || got.Code != tc.want):
				t.Fatalf("got %v, want %s", got, tc.want)
			}
		})
	}
}
