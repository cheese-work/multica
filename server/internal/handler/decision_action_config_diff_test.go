package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// E0b: the stored preview diffs raw text and line lists, and names its kind.

// previewOf runs the request-time core for a fixed read and decodes the stored
// preview. It needs no database: the fake Read ignores the queries.
func previewOf(test *testing.T, read configChangeRead) (stored map[string]any, preview map[string]json.RawMessage) {
	test.Helper()
	params, _ := json.Marshal(configChangeClaim{BaseSHA256: read.LiveSHA256, AfterSHA256: read.AfterSHA256})
	draft := &decisionActionDraft{Type: "test.diff", Params: params, Definition: &decisionaction.Definition{RequiresPreview: true}}
	change := configChange{Fields: []string{read.Field}, Read: func(context.Context, *db.Queries, pgtype.UUID, json.RawMessage) (configChangeRead, error) {
		return read, nil
	}}
	stored, err := configChangePreconditions(context.Background(), nil, pgtype.UUID{}, pgtype.UUID{}, draft, change)
	if err != nil {
		test.Fatal(err)
	}
	if err := json.Unmarshal(stored["preview"].(json.RawMessage), &preview); err != nil {
		test.Fatal(err)
	}
	return stored, preview
}

func textRead(before, after string) configChangeRead {
	return configChangeRead{
		Field: "workspace.context", TargetID: "t", Before: before, After: after,
		BeforeText: &before, AfterText: &after,
		LiveSHA256: decisionaction.TextDigest(before), AfterSHA256: decisionaction.TextDigest(after),
		HumanBlocksIntact: true,
	}
}

func decodeHunks(test *testing.T, preview map[string]json.RawMessage) []decisiontx.DiffHunk {
	test.Helper()
	var hunks []decisiontx.DiffHunk
	if err := json.Unmarshal(preview["diff"], &hunks); err != nil {
		test.Fatal(err)
	}
	return hunks
}

func changedLines(hunks []decisiontx.DiffHunk) (changed []string) {
	for _, hunk := range hunks {
		for _, line := range hunk.Lines {
			if line[0] != ' ' {
				changed = append(changed, line)
			}
		}
	}
	return changed
}

func TestTextDiffShowsOneEditedLineOfALongText(test *testing.T) {
	var lines []string
	for i := 0; i < 192; i++ {
		lines = append(lines, "line "+strings.Repeat("x", 44)+" "+string(rune('a'+i%26)))
	}
	before := strings.Join(lines, "\n")
	if n := len([]rune(before)); n < 9_900 || n > 10_100 {
		test.Fatalf("fixture has %d code points, want about 10,000", n)
	}
	lines[100] = "line EDITED"
	after := strings.Join(lines, "\n")
	_, preview := previewOf(test, textRead(before, after))
	hunks := decodeHunks(test, preview)
	if len(hunks) != 1 {
		test.Fatalf("hunks = %d, want 1", len(hunks))
	}
	if got := changedLines(hunks); len(got) != 2 || got[1] != "+line EDITED" {
		test.Fatalf("changed lines %q; want one removed and one added line", got)
	}
	if n := len(hunks[0].Lines); n != 6 {
		test.Fatalf("hunk has %d lines, want 2 changed + 4 context", n)
	}
}

func TestLineListDiffShowsOneAddAndOneRemove(test *testing.T) {
	before, after := []string{"a (1) [enabled]", "b (2) [enabled]", "c (3) [enabled]"}, []string{"a (1) [enabled]", "c (3) [enabled]", "d (4) [enabled]"}
	read := configChangeRead{
		Field: "workspace.context", TargetID: "t", Before: before, After: after,
		BeforeLines: before, AfterLines: after,
		LiveSHA256: "l", AfterSHA256: "a", HumanBlocksIntact: true,
	}
	_, preview := previewOf(test, read)
	if got := changedLines(decodeHunks(test, preview)); len(got) != 2 || got[0] != "-b (2) [enabled]" || got[1] != "+d (4) [enabled]" {
		test.Fatalf("changed lines %q; want -b and +d", got)
	}
	if string(preview["diff_kind"]) != `"lines"` {
		test.Fatalf("diff_kind = %s, want lines", preview["diff_kind"])
	}
}

func TestPreviewCarriesDiffKindAndMetaInsideTheDigest(test *testing.T) {
	read := textRead("a\nb", "a\nc")
	read.Meta = map[string]any{"unit": "code_points", "chars_before": 3, "chars_after": 3, "cap": 10000}
	stored, preview := previewOf(test, read)
	if string(preview["diff_kind"]) != `"text"` {
		test.Fatalf("diff_kind = %s, want text", preview["diff_kind"])
	}
	var meta map[string]any
	if err := json.Unmarshal(preview["meta"], &meta); err != nil || meta["unit"] != "code_points" || meta["cap"] != float64(10000) {
		test.Fatalf("meta %s (%v)", preview["meta"], err)
	}
	canonical, _ := decisionaction.Canonical(stored["preview"].(json.RawMessage))
	sum := sha256.Sum256(canonical)
	if stored["preview_sha256"] != hex.EncodeToString(sum[:]) {
		test.Fatal("preview_sha256 is not the digest of the stored preview")
	}
	read.Meta = map[string]any{"unit": "code_points", "chars_before": 3, "chars_after": 3, "cap": 9999}
	other, _ := previewOf(test, read)
	if other["preview_sha256"] == stored["preview_sha256"] {
		test.Fatal("meta is not covered by preview_sha256")
	}
}

func TestJSONPreviewKeepsItsDiffAndHasOnlyItsSectionInMeta(test *testing.T) {
	read := configChangeRead{
		Field: decisionaction.SettingsField, TargetID: "t", Keys: []string{"auto_compact"},
		Before: map[string]any{"auto_compact": false}, After: map[string]any{"auto_compact": true},
		LiveSHA256: "l", AfterSHA256: "a", HumanBlocksIntact: true,
	}
	_, preview := previewOf(test, read)
	if string(preview["diff_kind"]) != `"json"` {
		test.Fatalf("diff_kind = %s, want json", preview["diff_kind"])
	}
	var meta map[string]any
	if err := json.Unmarshal(preview["meta"], &meta); err != nil || len(meta) != 1 || meta["section"] == nil {
		test.Fatalf("settings preview meta %s (%v); want only the key section", preview["meta"], err)
	}
	if got := changedLines(decodeHunks(test, preview)); len(got) != 2 || got[0] != `-  "auto_compact": false` || got[1] != `+  "auto_compact": true` {
		test.Fatalf("changed lines %q", got)
	}
}

// A change that only touches line endings or the final newline has no changed
// line to show; the preview says so instead of showing an empty diff.
func TestTextDiffFlagsAWhitespaceOnlyChange(test *testing.T) {
	_, preview := previewOf(test, textRead("a\nb", "a\nb\n"))
	var meta map[string]any
	if err := json.Unmarshal(preview["meta"], &meta); err != nil || meta["whitespace_only"] != true {
		test.Fatalf("meta %s (%v); want whitespace_only", preview["meta"], err)
	}
}

// A text edit that also changes line endings or the final newline still shows
// its hunks, and the preview flags the change the line diff cannot show.
func TestTextDiffFlagsLineEndingChangesAlongsideHunks(test *testing.T) {
	cases := []struct {
		name, before, after string
		want                bool
	}{
		{"final newline added", "a\nb", "a\nc\n", true},
		{"final newline removed", "a\nb\n", "a\nc", true},
		{"crlf to lf", "a\r\nb\r\n", "a\nc\n", true},
		{"lf to crlf", "a\nb", "a\r\nc", true},
		{"same endings", "a\nb\n", "a\nc\n", false},
		{"same crlf endings", "a\r\nb", "a\r\nc", false},
		{"crlf file, line added", "a\r\nb\r\n", "a\r\nb\r\nc\r\n", false},
		{"crlf file, line removed", "a\r\nb\r\nc\r\n", "a\r\nc\r\n", false},
		{"lf file, line added", "a\nb\n", "a\nb\nc\n", false},
		{"crlf to mixed", "a\r\nb\r\n", "a\r\nc\n", true},
	}
	for _, c := range cases {
		test.Run(c.name, func(test *testing.T) {
			_, preview := previewOf(test, textRead(c.before, c.after))
			if len(decodeHunks(test, preview)) == 0 {
				test.Fatal("want hunks for the edited line")
			}
			var meta map[string]any
			if raw, has := preview["meta"]; has {
				if err := json.Unmarshal(raw, &meta); err != nil {
					test.Fatal(err)
				}
			}
			if got := meta["line_endings_changed"] == true; got != c.want {
				test.Fatalf("line_endings_changed = %v, want %v (meta %v)", got, c.want, meta)
			}
			if meta["whitespace_only"] == true {
				test.Fatal("whitespace_only is for a change with no hunks")
			}
		})
	}
}

func TestAgentInstructionsPreviewUsesTheTextDiffWithCodePointCounts(test *testing.T) {
	test.Skip("CHE-4873 quarantine (CHE-4909): POST /api/decisions returns governed_blocks_changed")
	const marks = "é \U0001F44D" // combining mark and emoji: 4 code points, 8 bytes
	request, _ := requestAgentInstructions(test, http.StatusCreated, func(params map[string]any) {
		after := governedAgentText + "\n" + marks
		params["instructions"], params["after_sha256"] = after, decisionaction.TextDigest(after)
	})
	var raw string
	dbfx.QueryRow(test, `SELECT preconditions->>'preview' FROM decision_action WHERE id = $1`, request.action.actionID).Scan(&raw)
	var preview map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &preview); err != nil {
		test.Fatal(err)
	}
	if string(preview["diff_kind"]) != `"text"` {
		test.Fatalf("diff_kind = %s, want text", preview["diff_kind"])
	}
	if got := changedLines(decodeHunks(test, preview)); len(got) != 1 || got[0] != "+"+marks {
		test.Fatalf("changed lines %q; want one added line, not a whole-string change", got)
	}
	var meta struct {
		Unit        string `json:"unit"`
		CharsBefore int    `json:"chars_before"`
		CharsAfter  int    `json:"chars_after"`
	}
	if err := json.Unmarshal(preview["meta"], &meta); err != nil {
		test.Fatal(err)
	}
	want := len([]rune(governedAgentText))
	if meta.Unit != "code_points" || meta.CharsBefore != want || meta.CharsAfter != want+1+4 {
		test.Fatalf("meta %+v; want code_points %d -> %d", meta, want, want+5)
	}
}

// Counts are Unicode code points, not bytes or UTF-16 units; digests stay over
// the UTF-8 bytes.
func TestTextConfigReadCountsCodePoints(test *testing.T) {
	const marks = "e\u0301 \U0001F44D" // e + combining mark, space, emoji: 4 code points, 8 bytes
	read := textConfigRead("workspace.context", "t", "ab", "ab"+marks)
	if read.Meta["unit"] != "code_points" || read.Meta["chars_before"] != 2 || read.Meta["chars_after"] != 6 {
		test.Fatalf("meta %v; want code_points 2 -> 6", read.Meta)
	}
	if read.AfterSHA256 != decisionaction.TextDigest("ab"+marks) || *read.AfterText != "ab"+marks {
		test.Fatal("digest or text does not match the after text")
	}
}

// CHE-5703: the preview names the section a change lands in, so Show full opens
// that section and not the whole value. The scope is inside the preview digest.

func sectionOf(test *testing.T, preview map[string]json.RawMessage) map[string]any {
	test.Helper()
	var meta map[string]any
	if raw, has := preview["meta"]; has {
		if err := json.Unmarshal(raw, &meta); err != nil {
			test.Fatal(err)
		}
	}
	section, _ := meta["section"].(map[string]any)
	return section
}

func sectionItems(test *testing.T, section map[string]any) []map[string]any {
	test.Helper()
	raw, _ := section["items"].([]any)
	items := make([]map[string]any, len(raw))
	for i, item := range raw {
		items[i], _ = item.(map[string]any)
	}
	return items
}

const sectionedText = "intro\n# Alpha\na1\na2\n# Review exception\nuntil 13 Nov\nkeep\n## Sub\ns1\n# Omega\no1\n"

func TestTextPreviewScopesAChangeToItsHeadingSection(test *testing.T) {
	after := strings.Replace(sectionedText, "until 13 Nov", "until 14 Nov\nOpus signs Tier 0-2 code", 1)
	_, preview := previewOf(test, textRead(sectionedText, after))
	section := sectionOf(test, preview)
	if section["kind"] != "heading" {
		test.Fatalf("section = %v, want kind heading", section)
	}
	items := sectionItems(test, section)
	if len(items) != 1 || items[0]["name"] != "Review exception" || items[0]["start_line"] != float64(5) || items[0]["end_line"] != float64(8) {
		test.Fatalf("items = %v, want Review exception lines 5 to 8 of the after text (heading included, up to the next heading)", items)
	}
}

func TestTextPreviewScopesARemovedLineToTheSectionItLeft(test *testing.T) {
	after := strings.Replace(sectionedText, "a2\n", "", 1)
	_, preview := previewOf(test, textRead(sectionedText, after))
	items := sectionItems(test, sectionOf(test, preview))
	if len(items) != 1 || items[0]["name"] != "Alpha" {
		test.Fatalf("items = %v, want Alpha only", items)
	}
}

func TestTextPreviewListsEveryChangedSectionOnce(test *testing.T) {
	after := strings.Replace(strings.Replace(sectionedText, "a1", "a1x", 1), "o1", "o1x", 1)
	_, preview := previewOf(test, textRead(sectionedText, after))
	items := sectionItems(test, sectionOf(test, preview))
	if len(items) != 2 || items[0]["name"] != "Alpha" || items[1]["name"] != "Omega" {
		test.Fatalf("items = %v, want Alpha then Omega", items)
	}
}

func TestTextPreviewHasNoScopeWhenAChangeSitsOutsideAnyHeading(test *testing.T) {
	_, preview := previewOf(test, textRead(sectionedText, strings.Replace(sectionedText, "intro", "intro2", 1)))
	if section := sectionOf(test, preview); section != nil {
		test.Fatalf("section = %v, want none: the intro has no heading", section)
	}
}

func TestTextPreviewIgnoresHeadingsInsideCodeFences(test *testing.T) {
	before := "# Real\nx\n```\n# not a heading\ny\n```\n"
	_, preview := previewOf(test, textRead(before, strings.Replace(before, "y", "z", 1)))
	items := sectionItems(test, sectionOf(test, preview))
	if len(items) != 1 || items[0]["name"] != "Real" {
		test.Fatalf("items = %v, want Real", items)
	}
}

func TestStepsPreviewScopesAChangeToTheChangedStep(test *testing.T) {
	step := func(key, handler string) IssueWorkflowStepPayload {
		return IssueWorkflowStepPayload{StatusKey: key, Handler: IssueWorkflowHandlerPayload{Type: "agent", ID: handler}}
	}
	read := configChangeRead{
		Field: "workflow.steps", TargetID: "t", LiveSHA256: "l", AfterSHA256: "a", HumanBlocksIntact: true,
		Before: []IssueWorkflowStepPayload{step("todo", "x"), step("agent_review", "old")},
		After:  []IssueWorkflowStepPayload{step("todo", "x"), step("agent_review", "new")},
	}
	_, preview := previewOf(test, read)
	section := sectionOf(test, preview)
	items := sectionItems(test, section)
	if section["kind"] != "step" || len(items) != 1 || items[0]["name"] != "agent_review" {
		test.Fatalf("section = %v, want step agent_review", section)
	}
}

func TestStepInstructionsPreviewNamesItsStep(test *testing.T) {
	read := configChangeRead{
		Field: "workflow.instructions", TargetID: "t", LiveSHA256: "l", AfterSHA256: "a", HumanBlocksIntact: true,
		Before: map[string]string{"status_key": "in_review", "instructions": "a"},
		After:  map[string]string{"status_key": "in_review", "instructions": "b"},
	}
	_, preview := previewOf(test, read)
	items := sectionItems(test, sectionOf(test, preview))
	if len(items) != 1 || items[0]["name"] != "in_review" {
		test.Fatalf("items = %v, want in_review", items)
	}
}

func TestSettingsPreviewScopesToItsKeys(test *testing.T) {
	read := configChangeRead{
		Field: decisionaction.SettingsField, TargetID: "t", Keys: []string{"auto_compact"},
		Before: map[string]any{"auto_compact": false}, After: map[string]any{"auto_compact": true},
		LiveSHA256: "l", AfterSHA256: "a", HumanBlocksIntact: true,
	}
	_, preview := previewOf(test, read)
	section := sectionOf(test, preview)
	items := sectionItems(test, section)
	if section["kind"] != "key" || len(items) != 1 || items[0]["name"] != "auto_compact" {
		test.Fatalf("section = %v, want key auto_compact", section)
	}
}

func TestSectionIsCoveredByThePreviewDigest(test *testing.T) {
	scoped, preview := previewOf(test, textRead(sectionedText, strings.Replace(sectionedText, "a1", "a1x", 1)))
	if sectionOf(test, preview) == nil {
		test.Fatal("section is not inside the stored preview")
	}
	canonical, _ := json.Marshal(scoped["preview"])
	if len(canonical) == 0 || scoped["preview_sha256"] == "" {
		test.Fatal("preview is not digested")
	}
	var withoutSection map[string]any
	if err := json.Unmarshal(scoped["preview"].(json.RawMessage), &withoutSection); err != nil {
		test.Fatal(err)
	}
	delete(withoutSection["meta"].(map[string]any), "section")
	raw, _ := json.Marshal(withoutSection)
	if previewDigest(raw) == scoped["preview_sha256"] {
		test.Fatal("dropping the section does not change the digest")
	}
}
