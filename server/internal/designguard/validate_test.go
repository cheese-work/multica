package designguard

import (
	"context"
	"errors"
	"testing"

	"github.com/multica-ai/multica/server/internal/doopread"
)

const (
	testOrigin   = "https://doop.example.com"
	testWorkflow = "11111111-1111-4111-8111-111111111111"
	testProperty = "22222222-2222-4222-8222-222222222222"
)

type fakeAttestations struct {
	views map[string]doopread.View
	errs  map[string]error
	calls map[string]int
}

func (f *fakeAttestations) Attestation(_ context.Context, id string) (doopread.View, error) {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[id]++
	if err := f.errs[id]; err != nil {
		return doopread.View{}, err
	}
	view, ok := f.views[id]
	if !ok {
		return doopread.View{}, doopread.ErrNotFound
	}
	return view, nil
}

type fakeVerdicts struct {
	verdict Verdict
	found   bool
	err     error
	asked   []string
}

func (f *fakeVerdicts) NewestDesignVerdict(_ context.Context, issueID, attestationID string) (Verdict, bool, error) {
	f.asked = append(f.asked, issueID+"/"+attestationID)
	return f.verdict, f.found, f.err
}

func str(s string) *string { return &s }

// baseView is a complete, current attestation of page p1 on canvas cv1, bound to issue-1,
// written by the Anthropic agent A.
func baseView() doopread.View {
	return doopread.View{
		CanvasID: "cv1", PageID: "p1", PageIssueID: "issue-1", Complete: true, Current: true,
		Frames:             []doopread.Frame{{FrameID: "f1", UpdatedAt: 111}, {FrameID: "f2", UpdatedAt: 222}},
		Authors:            []doopread.Author{{Kind: "agent", AgentID: str("A"), TaskID: str("ta")}},
		ProvenanceComplete: true,
	}
}

type scenario struct {
	in       Input
	view     doopread.View
	viewErr  error
	attested *fakeAttestations
	verdicts *fakeVerdicts
}

// newScenario is a card that passes: one option citing f1, reviewed PASS by the OpenAI agent S, sent by B.
func newScenario() *scenario {
	return &scenario{
		in: Input{
			Settings: Settings{Mode: ModeBlock, DoopOrigin: testOrigin, DesignURLPropertyID: testProperty, DesignWorkflowID: testWorkflow},
			IssueID:  "issue-1", DesignURL: testOrigin + "/c/cv1?page=p1", Class: ClassProductDirection, AskingAgentID: "B", Plain: true,
			Options: []Option{
				{Label: "One", WhatHappens: "Ship one.", Design: &OptionDesign{FrameURL: testOrigin + "/c/cv1?frame=f1", AttestationID: "run-1"}},
			},
		},
		view:     baseView(),
		verdicts: &fakeVerdicts{verdict: Verdict{Verdict: "PASS", AgentID: "S", TaskID: "ts"}, found: true},
	}
}

func (s *scenario) run() (Result, *Refusal) {
	s.attested = &fakeAttestations{views: map[string]doopread.View{"run-1": s.view}, errs: map[string]error{}, calls: map[string]int{}}
	if s.viewErr != nil {
		s.attested.errs["run-1"] = s.viewErr
	}
	return Validate(context.Background(), s.in, ValidateDeps{Attestations: s.attested, Verdicts: s.verdicts, Independence: fakeDeps()})
}

func (s *scenario) want(t *testing.T, code string) {
	t.Helper()
	result, refusal := s.run()
	switch {
	case code == "" && refusal != nil:
		t.Fatalf("refused %s (%s), want a pass", refusal.Code, refusal.Message)
	case code != "" && refusal == nil:
		t.Fatalf("passed with %+v, want %s", result, code)
	case code != "" && refusal.Code != code:
		t.Fatalf("refused %s (%s), want %s", refusal.Code, refusal.Message, code)
	}
}

func TestValidatePassesAndReturnsThePerOptionResult(t *testing.T) {
	s := newScenario()
	s.in.Options = append(s.in.Options, Option{Label: "Two", Design: &OptionDesign{FrameURL: testOrigin + "/c/cv1/proto?frame=f2&page=p1", AttestationID: "run-1"}})
	result, refusal := s.run()
	if refusal != nil {
		t.Fatalf("refused %v", refusal)
	}
	want := []Checked{
		{FrameID: "f1", CanvasID: "cv1", PageID: "p1", AttestationID: "run-1", FrameUpdatedAt: 111},
		{FrameID: "f2", CanvasID: "cv1", PageID: "p1", AttestationID: "run-1", FrameUpdatedAt: 222},
	}
	if !result.Triggered || len(result.Options) != 2 || result.Options[0] != want[0] || result.Options[1] != want[1] {
		t.Fatalf("result = %+v, want triggered with %+v", result, want)
	}
	if s.attested.calls["run-1"] != 1 {
		t.Fatalf("doop read %d times, want once for one attestation", s.attested.calls["run-1"])
	}
	if len(s.verdicts.asked) != 1 {
		t.Fatalf("verdict read %d times, want once", len(s.verdicts.asked))
	}
}

func TestValidateRefusalCodes(t *testing.T) {
	url := func(s string) string { return testOrigin + s }
	cases := []struct {
		name   string
		mutate func(s *scenario)
		code   string
	}{
		{"C1 no design on the second option", func(s *scenario) {
			s.in.Options = append(s.in.Options, Option{Label: "Two"})
		}, CodeDesignOptionMissing},
		{"C1 frame_url without a frame", func(s *scenario) { s.in.Options[0].Design.FrameURL = url("/c/cv1?page=p1") }, CodeDesignFrameURLInvalid},
		{"C1 frame_url off the origin", func(s *scenario) {
			s.in.Options[0].Design.FrameURL = "https://other.example.com/c/cv1?frame=f1"
		}, CodeDesignFrameURLInvalid},
		{"C1 repeated frame", func(s *scenario) { s.in.Options[0].Design.FrameURL = url("/c/cv1?frame=f1&frame=f2") }, CodeDesignURLAmbiguous},
		{"C2 Design url unset", func(s *scenario) { s.in.DesignURL = "" }, CodeDesignURLMissing},
		{"C2 Design url off the origin", func(s *scenario) { s.in.DesignURL = "https://other.example.com/c/cv1?page=p1" }, CodeDesignURLMissing},
		{"C2 Design url repeats page", func(s *scenario) { s.in.DesignURL = url("/c/cv1?page=p1&page=p2") }, CodeDesignURLAmbiguous},
		{"C3 only the option canvas changed (review r1)", func(s *scenario) {
			s.in.Options[0].Design.FrameURL = url("/c/cv2?frame=f1")
		}, CodeDesignCanvasMismatch},
		{"C3 Design url canvas differs (review r1)", func(s *scenario) { s.in.DesignURL = url("/c/cv2?page=p1") }, CodeDesignCanvasMismatch},
		{"C3 attestation canvas differs", func(s *scenario) { s.view.CanvasID = "cv9" }, CodeDesignCanvasMismatch},
		{"C4 Design url page differs", func(s *scenario) { s.in.DesignURL = url("/c/cv1?page=p9") }, CodeDesignFrameOutsidePage},
		{"C4 Design url frame is not in the attestation", func(s *scenario) { s.in.DesignURL = url("/c/cv1?frame=f9") }, CodeDesignFrameOutsidePage},
		{"C4 option page differs", func(s *scenario) { s.in.Options[0].Design.FrameURL = url("/c/cv1?frame=f1&page=p9") }, CodeDesignFrameOutsidePage},
		{"C4 Design url page right and frame wrong", func(s *scenario) { s.in.DesignURL = url("/c/cv1?page=p1&frame=f9") }, CodeDesignFrameOutsidePage},
		{"C4b page bound to no issue", func(s *scenario) { s.view.PageIssueID = "" }, CodeDesignPageNotBound},
		{"C4b page bound to another issue", func(s *scenario) { s.view.PageIssueID = "issue-2" }, CodeDesignPageNotBound},
		{"C5 attestation not found", func(s *scenario) { s.viewErr = doopread.ErrNotFound }, CodeDesignAttestationMissing},
		{"C5 attestation incomplete", func(s *scenario) { s.view.Complete = false }, CodeDesignAttestationPartial},
		{"C5 attestation stale (edit after PASS)", func(s *scenario) { s.view.Current = false }, CodeDesignAttestationStale},
		{"C5 page right but frame not attested (review r1)", func(s *scenario) {
			s.in.Options[0].Design.FrameURL = url("/c/cv1?frame=f9")
		}, CodeDesignFrameNotAttested},
		{"doop unreachable", func(s *scenario) { s.viewErr = doopread.ErrUnavailable }, CodeDesignDoopUnavailable},
		{"doop not configured", func(s *scenario) { s.viewErr = doopread.ErrNotConfigured }, CodeDesignDoopUnavailable},
		{"doop gives an unexpected error", func(s *scenario) { s.viewErr = errors.New("boom") }, CodeDesignDoopUnavailable},
		{"C6 no verdict", func(s *scenario) { s.verdicts.found = false }, CodeDesignVerdictMissing},
		{"C6 newest verdict is CHANGES", func(s *scenario) { s.verdicts.verdict.Verdict = "CHANGES" }, CodeDesignVerdictMissing},
		{"C6 verdict lookup fails", func(s *scenario) { s.verdicts.err = errors.New("db down") }, CodeGuardUnavailable},
		{"C7 self review through another submitter (review r1)", func(s *scenario) {
			s.verdicts.verdict = Verdict{Verdict: "PASS", AgentID: "A", TaskID: "ta"}
		}, CodeSelfReview},
		{"C7 same-lab review through another submitter (review r1)", func(s *scenario) {
			s.verdicts.verdict = Verdict{Verdict: "PASS", AgentID: "A2", TaskID: "ta2"}
		}, CodeSameLab},
		{"C7 reviewer sends the card", func(s *scenario) { s.in.AskingAgentID = "S" }, CodeSameAgentAsAsker},
		{"C7 provenance incomplete", func(s *scenario) { s.view.ProvenanceComplete = false }, CodeAuthorUnknown},
		{"C7 agent author without a task id", func(s *scenario) {
			s.view.Authors = []doopread.Author{{Kind: "agent", AgentID: str("A"), TaskID: str("")}}
		}, CodeAuthorUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newScenario()
			tc.mutate(s)
			s.want(t, tc.code)
		})
	}
}

func TestValidateAcceptsCaseVariantsThatMustPass(t *testing.T) {
	url := func(s string) string { return testOrigin + s }
	cases := map[string]func(s *scenario){
		"Design url names a frame of the page":  func(s *scenario) { s.in.DesignURL = url("/c/cv1?frame=f2") },
		"Design url names page and frame":       func(s *scenario) { s.in.DesignURL = url("/c/cv1?page=p1&frame=f1") },
		"option page agrees":                    func(s *scenario) { s.in.Options[0].Design.FrameURL = url("/c/cv1?frame=f1&page=p1") },
		"proto path":                            func(s *scenario) { s.in.Options[0].Design.FrameURL = url("/c/cv1/proto?frame=f1") },
		"a user author is outside A3 to A5":     func(s *scenario) { s.view.Authors = append(s.view.Authors, doopread.Author{Kind: "user"}) },
		"two options on the same frame":         func(s *scenario) { s.in.Options = append(s.in.Options, s.in.Options[0]) },
		"another query parameter is ignored":    func(s *scenario) { s.in.Options[0].Design.FrameURL = url("/c/cv1?frame=f1&utm=x") },
		"verdict recorded after author task":    func(s *scenario) { s.verdicts.verdict = Verdict{Verdict: "PASS", AgentID: "S", TaskID: "ts"} },
		"class reversible with a design option": func(s *scenario) { s.in.Class = "reversible" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := newScenario()
			mutate(s)
			s.want(t, "")
		})
	}
}

func TestValidateChecksEveryOption(t *testing.T) {
	s := newScenario()
	s.in.Options = append(s.in.Options, Option{Label: "Two", Design: &OptionDesign{FrameURL: testOrigin + "/c/cv1?frame=f9", AttestationID: "run-1"}})
	s.want(t, CodeDesignFrameNotAttested)

	s = newScenario()
	s.in.Options = append(s.in.Options, Option{Label: "Two", Design: &OptionDesign{FrameURL: testOrigin + "/c/cv1?frame=f2", AttestationID: "run-2"}})
	s.want(t, CodeDesignAttestationMissing)
}

func TestTriggerTable(t *testing.T) {
	link := testOrigin + "/c/cv1?frame=f1"
	cases := []struct {
		name   string
		mutate func(in *Input)
		want   bool
	}{
		{"T0 option carries design, any class", func(in *Input) { in.Class = "reversible" }, true},
		{"T0 option carries design, deploy", func(in *Input) { in.Class = "deploy" }, true},
		{"T1 Design url on the doop origin", func(in *Input) {
			in.Options[0].Design = nil
		}, true},
		{"T1 Design url off the origin", func(in *Input) {
			in.Options[0].Design = nil
			in.DesignURL = "https://other.example.com/c/cv1?page=p1"
		}, false},
		{"T1 Design url on the origin but class reversible", func(in *Input) {
			in.Options[0].Design = nil
			in.Class = "reversible"
		}, false},
		{"T2 issue on the Design workflow", func(in *Input) {
			in.Options[0].Design = nil
			in.DesignURL = ""
			in.IssueWorkflowID = testWorkflow
		}, true},
		{"T2 another workflow", func(in *Input) {
			in.Options[0].Design = nil
			in.DesignURL = ""
			in.IssueWorkflowID = "33333333-3333-4333-8333-333333333333"
		}, false},
		{"T2 no Design workflow configured", func(in *Input) {
			in.Options[0].Design = nil
			in.DesignURL = ""
			in.IssueWorkflowID = ""
			in.Settings.DesignWorkflowID = ""
		}, false},
		{"T3 label holds a doop link", func(in *Input) {
			in.Options[0].Design, in.DesignURL = nil, ""
			in.Options[0].Label = "Pick " + link
		}, true},
		{"T3 what_happens holds a doop link", func(in *Input) {
			in.Options[0].Design, in.DesignURL = nil, ""
			in.Options[0].WhatHappens = "See [frame](" + link + ")."
		}, true},
		{"T3 the host name as a word is not a link", func(in *Input) {
			in.Options[0].Design, in.DesignURL = nil, ""
			in.Options[0].WhatHappens = "Open doop.example.com and look."
		}, false},
		{"T3 a link off the origin", func(in *Input) {
			in.Options[0].Design, in.DesignURL = nil, ""
			in.Options[0].WhatHappens = "See https://other.example.com/c/x?frame=f."
		}, false},
		{"not a plain decision card", func(in *Input) { in.Plain = false }, false},
		{"nothing points at doop", func(in *Input) {
			in.Options[0].Design, in.DesignURL = nil, ""
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newScenario()
			tc.mutate(&s.in)
			if got := Triggers(s.in); got != tc.want {
				t.Fatalf("Triggers = %v, want %v", got, tc.want)
			}
			result, refusal := s.run()
			if !tc.want && (refusal != nil || result.Triggered || len(result.Options) != 0) {
				t.Fatalf("untriggered card gave %+v, %v; want an empty pass", result, refusal)
			}
			if !tc.want && (len(s.attested.calls) != 0 || len(s.verdicts.asked) != 0) {
				t.Fatalf("untriggered card read doop %v or verdicts %v", s.attested.calls, s.verdicts.asked)
			}
		})
	}
}

func TestTriggeredCardWithoutADesignOptionIsRefused(t *testing.T) {
	// T1: the Design url names doop, so a product_direction card must cite a frame.
	s := newScenario()
	s.in.Options[0].Design = nil
	s.want(t, CodeDesignOptionMissing)
	// T3: a link in the option text alone triggers, then C1 refuses.
	s = newScenario()
	s.in.Options[0].Design, s.in.DesignURL = nil, ""
	s.in.Options[0].Label = "Pick " + testOrigin + "/c/cv1?frame=f1"
	s.want(t, CodeDesignOptionMissing)
}
