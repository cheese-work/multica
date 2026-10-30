package decisiontx

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const (
	testAttachmentID = "33333333-3333-4333-8333-333333333333"
	testPrevID       = "44444444-4444-4444-8444-444444444444"
	testSHA          = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
)

func artifactMap() map[string]any {
	return map[string]any{
		"attachment_id": testAttachmentID, "label": "Design rev 5", "sha256": testSHA,
		"supersedes_attachment_id": testPrevID,
		"signed_by":                map[string]any{"reviewer": "x99-codex-sol", "comment_id": testCommentID},
	}
}

func TestDecodeAskArtifacts(test *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{name: "valid", mutate: func(ask map[string]any) {
			ask["subject"] = "artifact_approval"
			ask["artifacts"] = []any{artifactMap()}
		}},
		{name: "no subject, no artifacts"},
		{name: "artifacts without subject", mutate: func(ask map[string]any) { ask["artifacts"] = []any{artifactMap()} }},
		{name: "artifact_approval without artifacts", mutate: func(ask map[string]any) { ask["subject"] = "artifact_approval" }, want: "ask.artifacts requires at least one artifact when subject is artifact_approval"},
		{name: "artifact_approval with empty list", mutate: func(ask map[string]any) { ask["subject"] = "artifact_approval"; ask["artifacts"] = []any{} }, want: "ask.artifacts requires at least one artifact when subject is artifact_approval"},
		{name: "unknown subject", mutate: func(ask map[string]any) { ask["subject"] = "other" }, want: "ask.subject must be artifact_approval when set"},
		{name: "artifacts not a list", mutate: func(ask map[string]any) { ask["artifacts"] = "x" }, want: "ask.artifacts must be a list"},
		{name: "unknown artifact field", mutate: func(ask map[string]any) { a := artifactMap(); a["url"] = "x"; ask["artifacts"] = []any{a} }, want: "ask.artifacts[0].url is not a known field"},
		{name: "bad attachment id", mutate: func(ask map[string]any) { a := artifactMap(); a["attachment_id"] = "nope"; ask["artifacts"] = []any{a} }, want: "ask.artifacts[0].attachment_id must be a UUID"},
		{name: "blank label", mutate: func(ask map[string]any) { a := artifactMap(); a["label"] = " "; ask["artifacts"] = []any{a} }, want: "ask.artifacts[0].label is required"},
		{name: "short sha", mutate: func(ask map[string]any) { a := artifactMap(); a["sha256"] = "abc"; ask["artifacts"] = []any{a} }, want: "ask.artifacts[0].sha256 must be 64 lowercase hex characters"},
		{name: "bad supersedes", mutate: func(ask map[string]any) {
			a := artifactMap()
			a["supersedes_attachment_id"] = "x"
			ask["artifacts"] = []any{a}
		}, want: "ask.artifacts[0].supersedes_attachment_id must be a UUID"},
		{name: "signed_by needs reviewer", mutate: func(ask map[string]any) {
			a := artifactMap()
			a["signed_by"] = map[string]any{"comment_id": testCommentID}
			ask["artifacts"] = []any{a}
		}, want: "ask.artifacts[0].signed_by.reviewer is required"},
		{name: "signed_by bad comment", mutate: func(ask map[string]any) {
			a := artifactMap()
			a["signed_by"] = map[string]any{"reviewer": "sol", "comment_id": "x"}
			ask["artifacts"] = []any{a}
		}, want: "ask.artifacts[0].signed_by.comment_id must be a UUID"},
		{name: "duplicate attachment", mutate: func(ask map[string]any) { ask["artifacts"] = []any{artifactMap(), artifactMap()} }, want: "ask.artifacts[1].attachment_id must differ from the other artifacts"},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			ask := validAskMap()
			if tc.mutate != nil {
				tc.mutate(ask)
			}
			raw, _ := json.Marshal(ask)
			if _, err := DecodeAsk(raw); errString(err) != tc.want {
				test.Fatalf("error = %q, want %q", errString(err), tc.want)
			}
		})
	}
}

func TestAskWithoutArtifactsKeepsStoredShape(test *testing.T) {
	raw, _ := json.Marshal(validAskMap())
	ask, err := DecodeAsk(raw)
	if err != nil {
		test.Fatal(err)
	}
	stored, _ := json.Marshal(ask)
	if strings.Contains(string(stored), "artifacts") || strings.Contains(string(stored), "subject") {
		test.Fatalf("ask without artifacts stores new keys: %s", stored)
	}
}

func numbered(count int) []string {
	lines := make([]string, count)
	for index := range lines {
		lines[index] = fmt.Sprintf("line %d", index+1)
	}
	return lines
}

func TestDiffReturnsOnlyChangedHunks(test *testing.T) {
	before := numbered(30)
	after := slicesClone(before)
	after[2] = "line 3 changed"
	after = append(after[:25], after[26:]...) // delete line 26
	hunks, err := Diff(strings.Join(before, "\n")+"\n", strings.Join(after, "\n")+"\n")
	if err != nil {
		test.Fatal(err)
	}
	if len(hunks) != 2 {
		test.Fatalf("hunks = %+v, want 2", hunks)
	}
	first, second := strings.Join(hunks[0].Lines, "|"), strings.Join(hunks[1].Lines, "|")
	if !strings.Contains(first, "-line 3|+line 3 changed") || !strings.Contains(second, "-line 26") {
		test.Fatalf("wrong hunks: %q / %q", first, second)
	}
	if hunks[0].OldStart != 1 || hunks[1].OldStart != 24 {
		test.Fatalf("starts = %d, %d; want 1, 24", hunks[0].OldStart, hunks[1].OldStart)
	}
	for _, far := range []string{"line 10", "line 15", "line 20"} {
		if strings.Contains(first+second, far) {
			test.Fatalf("unchanged %q leaked into the diff: %q / %q", far, first, second)
		}
	}
}

func TestDiffIdenticalAndAdjacentChanges(test *testing.T) {
	hunks, err := Diff("a\nb\n", "a\nb\n")
	if err != nil || hunks == nil || len(hunks) != 0 {
		test.Fatalf("identical = %+v, %v; want empty non-nil", hunks, err)
	}
	hunks, err = Diff("a\nb\nc\n", "a\nB\nc\nd\n")
	if err != nil || len(hunks) != 1 || strings.Join(hunks[0].Lines, "|") != " a|-b|+B| c|+d" {
		test.Fatalf("merged hunk = %+v, %v", hunks, err)
	}
	hunks, err = Diff("", "new\n")
	if err != nil || len(hunks) != 1 || strings.Join(hunks[0].Lines, "|") != "+new" {
		test.Fatalf("from empty = %+v, %v", hunks, err)
	}
}

func TestDiffRefusesHugeRewrite(test *testing.T) {
	_, err := Diff(strings.Join(numbered(3000), "\n"), strings.Join(numbered(3000), "x\n")+"x")
	if !errors.Is(err, ErrDiffTooLarge) {
		test.Fatalf("err = %v, want ErrDiffTooLarge", err)
	}
}

func slicesClone(lines []string) []string { return append([]string(nil), lines...) }
