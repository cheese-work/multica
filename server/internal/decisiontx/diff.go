package decisiontx

import (
	"errors"
	"strings"
)

const (
	diffContext  = 2
	maxDiffCells = 4_000_000
)

// ErrDiffTooLarge means the changed middle of two files is too big to compare.
var ErrDiffTooLarge = errors.New("files differ too much to diff")

// DiffHunk is one changed region with up to two lines of context. Each line
// starts with " ", "-" or "+"; the starts are 1-based line numbers.
type DiffHunk struct {
	OldStart int      `json:"old_start"`
	NewStart int      `json:"new_start"`
	Lines    []string `json:"lines"`
}

type diffOp struct {
	kind     byte
	text     string
	old, new int
}

// Diff returns only the changed hunks between two texts, never the whole file.
// ponytail: O(changed lines squared) LCS after trimming the shared head and
// tail; ErrDiffTooLarge above maxDiffCells. Swap in Myers if rewrites matter.
func Diff(oldText, newText string) ([]DiffHunk, error) {
	a, b := splitLines(oldText), splitLines(newText)
	head := 0
	for head < len(a) && head < len(b) && a[head] == b[head] {
		head++
	}
	tail := 0
	for tail < len(a)-head && tail < len(b)-head && a[len(a)-1-tail] == b[len(b)-1-tail] {
		tail++
	}
	midA, midB := a[head:len(a)-tail], b[head:len(b)-tail]
	if len(midA)*len(midB) > maxDiffCells {
		return nil, ErrDiffTooLarge
	}
	var ops []diffOp
	oldNo, newNo := 1, 1
	emit := func(kind byte, text string) {
		ops = append(ops, diffOp{kind, text, oldNo, newNo})
		if kind != '+' {
			oldNo++
		}
		if kind != '-' {
			newNo++
		}
	}
	for _, line := range a[:head] {
		emit(' ', line)
	}
	width := len(midB) + 1
	lcs := make([]int32, (len(midA)+1)*width)
	for i := len(midA) - 1; i >= 0; i-- {
		for j := len(midB) - 1; j >= 0; j-- {
			if midA[i] == midB[j] {
				lcs[i*width+j] = lcs[(i+1)*width+j+1] + 1
			} else {
				lcs[i*width+j] = max(lcs[(i+1)*width+j], lcs[i*width+j+1])
			}
		}
	}
	i, j := 0, 0
	for i < len(midA) || j < len(midB) {
		switch {
		case i < len(midA) && j < len(midB) && midA[i] == midB[j]:
			emit(' ', midA[i])
			i++
			j++
		case i < len(midA) && (j == len(midB) || lcs[(i+1)*width+j] >= lcs[i*width+j+1]):
			emit('-', midA[i])
			i++
		default:
			emit('+', midB[j])
			j++
		}
	}
	for _, line := range a[len(a)-tail:] {
		emit(' ', line)
	}
	return groupHunks(ops), nil
}

func groupHunks(ops []diffOp) []DiffHunk {
	hunks := []DiffHunk{}
	var changed []int
	for index, op := range ops {
		if op.kind != ' ' {
			changed = append(changed, index)
		}
	}
	for next := 0; next < len(changed); {
		first, last := changed[next], changed[next]
		for next++; next < len(changed) && changed[next]-last <= 2*diffContext; next++ {
			last = changed[next]
		}
		from, to := max(first-diffContext, 0), min(last+diffContext, len(ops)-1)
		hunk := DiffHunk{OldStart: ops[from].old, NewStart: ops[from].new}
		for _, op := range ops[from : to+1] {
			hunk.Lines = append(hunk.Lines, string(op.kind)+op.text)
		}
		hunks = append(hunks, hunk)
	}
	return hunks
}

func splitLines(text string) []string {
	text = strings.TrimSuffix(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}
