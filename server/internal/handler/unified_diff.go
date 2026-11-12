package handler

import (
	"errors"
	"fmt"
	"strings"
)

// maxDiffCells bounds the LCS table (4M cells, 16 MB of int32).
const maxDiffCells = 4_000_000

var errDiffTooLarge = errors.New("description too large to diff")

type diffOp struct {
	kind byte // ' ', '-', '+'
	text string
}

func splitDiffLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// unifiedDiff returns a unified diff with 3 lines of context, or "" when a
// and b are equal. ponytail: LCS table over the changed middle only, capped at
// maxDiffCells; swap in Myers if descriptions ever outgrow the cap.
func unifiedDiff(a, b, fromName, toName string) (string, error) {
	return unifiedDiffContext(a, b, fromName, toName, 3)
}

// unifiedDiffContext is unifiedDiff with ctx lines of unchanged context around
// each change; a huge ctx returns the whole file as one hunk.
func unifiedDiffContext(a, b, fromName, toName string, ctx int) (string, error) {
	al, bl := splitDiffLines(a), splitDiffLines(b)
	pre := 0
	for pre < len(al) && pre < len(bl) && al[pre] == bl[pre] {
		pre++
	}
	suf := 0
	for suf < len(al)-pre && suf < len(bl)-pre && al[len(al)-1-suf] == bl[len(bl)-1-suf] {
		suf++
	}
	am, bm := al[pre:len(al)-suf], bl[pre:len(bl)-suf]
	if len(am) == 0 && len(bm) == 0 {
		return "", nil
	}
	if (len(am)+1)*(len(bm)+1) > maxDiffCells {
		return "", errDiffTooLarge
	}

	// lcs[i][j] = LCS length of am[i:], bm[j:].
	lcs := make([][]int32, len(am)+1)
	for i := range lcs {
		lcs[i] = make([]int32, len(bm)+1)
	}
	for i := len(am) - 1; i >= 0; i-- {
		for j := len(bm) - 1; j >= 0; j-- {
			if am[i] == bm[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	ops := make([]diffOp, 0, len(al)+len(bl))
	for _, l := range al[:pre] {
		ops = append(ops, diffOp{' ', l})
	}
	i, j := 0, 0
	for i < len(am) || j < len(bm) {
		switch {
		case i < len(am) && j < len(bm) && am[i] == bm[j]:
			ops = append(ops, diffOp{' ', am[i]})
			i, j = i+1, j+1
		case j == len(bm) || (i < len(am) && lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, diffOp{'-', am[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', bm[j]})
			j++
		}
	}
	for _, l := range al[len(al)-suf:] {
		ops = append(ops, diffOp{' ', l})
	}

	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", fromName, toName)
	oldLine, newLine := 1, 1 // line numbers at ops[k]
	k := 0
	for k < len(ops) {
		if ops[k].kind == ' ' {
			oldLine, newLine, k = oldLine+1, newLine+1, k+1
			continue
		}
		// Hunk start: back up to ctx lines of leading context.
		start := k
		for start > 0 && k-start < ctx && ops[start-1].kind == ' ' {
			start--
		}
		oldStart, newStart := oldLine-(k-start), newLine-(k-start)
		// Extend until ctx*2+1 consecutive unchanged lines or the end.
		end, run := k, 0
		for end < len(ops) && run <= 2*ctx {
			if ops[end].kind == ' ' {
				run++
			} else {
				run = 0
			}
			end++
		}
		// Drop the unchanged tail beyond ctx.
		for run > ctx {
			end--
			run--
		}
		oldN, newN := 0, 0
		for _, op := range ops[start:end] {
			if op.kind != '+' {
				oldN++
			}
			if op.kind != '-' {
				newN++
			}
		}
		if oldN == 0 {
			oldStart--
		}
		if newN == 0 {
			newStart--
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", oldStart, oldN, newStart, newN)
		for _, op := range ops[start:end] {
			out.WriteByte(op.kind)
			out.WriteString(op.text)
			out.WriteByte('\n')
		}
		for _, op := range ops[k:end] {
			if op.kind != '+' {
				oldLine++
			}
			if op.kind != '-' {
				newLine++
			}
		}
		k = end
	}
	return out.String(), nil
}
