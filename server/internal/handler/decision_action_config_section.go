package handler

import (
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/decisiontx"
)

// Section scope (CHE-5703). A long config shows its changed rows by default; Show
// full opens the value as written. The preview names the section the change lands
// in, so Show full opens only that section: a workflow step (kind "step", by status
// key), a text section (kind "heading", with its line range in the after text) or
// a settings key (kind "key"). The server decides the scope and stores it in the
// preview's meta, inside the preview digest. A change the server cannot place in a
// section carries no scope, and the client then opens the whole value.

// headingLine is an ATX Markdown heading outside a code fence.
var headingLine = regexp.MustCompile(`^ {0,3}#{1,6}[ \t]+(.*\S)[ \t]*$`)

// previewSection returns the section scope for a read, or nil when there is none.
func previewSection(read configChangeRead, kind string, hunks []decisiontx.DiffHunk) map[string]any {
	if read.Field == decisionaction.SettingsField && len(read.Keys) > 0 {
		return namedSection("key", slices.Sorted(slices.Values(read.Keys)))
	}
	switch after := read.After.(type) {
	case []IssueWorkflowStepPayload:
		before, _ := read.Before.([]IssueWorkflowStepPayload)
		var changed []string
		for _, step := range after {
			index := slices.IndexFunc(before, func(old IssueWorkflowStepPayload) bool { return old.StatusKey == step.StatusKey })
			if index < 0 || !reflect.DeepEqual(before[index], step) {
				changed = append(changed, step.StatusKey)
			}
		}
		return namedSection("step", changed)
	case map[string]string:
		if key := after["status_key"]; key != "" {
			return namedSection("step", []string{key})
		}
	}
	if kind == diffKindText && read.AfterText != nil {
		return headingSection(*read.AfterText, hunks)
	}
	return nil
}

func namedSection(kind string, names []string) map[string]any {
	if len(names) == 0 {
		return nil
	}
	items := make([]any, len(names))
	for i, name := range names {
		items[i] = map[string]any{"name": name}
	}
	return map[string]any{"kind": kind, "items": items}
}

// headingSection places every changed line of a text diff in the heading section
// that holds it: from the heading line to the line before the next heading. A
// removed line belongs to the section of the line above it. A change before the
// first heading has no section, and then no scope is returned.
func headingSection(after string, hunks []decisiontx.DiffHunk) map[string]any {
	lines := strings.Split(strings.ReplaceAll(after, "\r\n", "\n"), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	type heading struct {
		line int
		name string
	}
	var headings []heading
	fenced := false
	for i, text := range lines {
		trimmed := strings.TrimLeft(text, " ")
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			continue
		}
		if match := headingLine.FindStringSubmatch(text); !fenced && match != nil {
			headings = append(headings, heading{i + 1, match[1]})
		}
	}
	touched := map[int]bool{}
	for _, hunk := range hunks {
		line := hunk.NewStart
		for _, text := range hunk.Lines {
			switch text[0] {
			case ' ':
				line++
			case '+':
				touched[sectionAt(headings, line, func(h heading) int { return h.line })] = true
				line++
			case '-':
				touched[sectionAt(headings, max(line-1, 1), func(h heading) int { return h.line })] = true
			}
		}
	}
	if len(touched) == 0 || touched[-1] {
		return nil
	}
	var items []any
	for index, h := range headings {
		if !touched[index] {
			continue
		}
		end := len(lines)
		if index+1 < len(headings) {
			end = headings[index+1].line - 1
		}
		items = append(items, map[string]any{"name": h.name, "start_line": h.line, "end_line": end})
	}
	return map[string]any{"kind": "heading", "items": items}
}

// sectionAt is the index of the last heading at or above line, or -1.
func sectionAt[T any](headings []T, line int, lineOf func(T) int) int {
	found := -1
	for i, h := range headings {
		if lineOf(h) > line {
			break
		}
		found = i
	}
	return found
}
