// Package markdownlinks extracts the clickable destinations from a CommonMark
// body, so every guard that reasons about "what does this text actually link
// to" agrees on the answer.
//
// The parse-don't-scan choice is the whole point. A full-text scan cannot tell
// a link an agent is PUBLISHING from a path or URL it is DISCUSSING, and the
// bodies most likely to discuss one are exactly the bodies explaining a link
// defect. CommonMark draws that line for us: inline parsing does not run inside
// code spans or fenced blocks, so a destination quoted as code is structurally
// invisible here and needs no special case.
//
// Two callers, opposite questions: the MUL-4899 local-path lint asks which
// destinations must NOT be there, and the CHE-408 source-link guard asks
// whether a required destination IS there. Both need the same extraction, and
// the shared package is what keeps a fix to the walk from landing in only one.
package markdownlinks

import (
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// Reference is one clickable destination found in a body.
//
// Only the destination is recorded — never the link text. Text is author-
// controlled prose that can claim anything ("the authoritative plan"), so a
// guard that trusted it would accept a link whose label lies about where it
// goes.
type Reference struct {
	// Destination is the raw target as written: a URL, or whatever else the
	// author put there. Interpreting it is the caller's job.
	Destination string

	// Quoted reports that every occurrence of this destination sits inside a
	// blockquote. Unlike code spans, a blockquote does NOT suppress inline
	// parsing, so quoted links reach this walk like any other — and a caller
	// asking "did the author cite their own source" must not accept a link the
	// author merely quoted from someone else. Callers that don't care about
	// provenance can ignore this field.
	Quoted bool
}

// Find parses body as CommonMark and returns every link, image, and autolink
// destination, de-duplicated by destination in first-appearance order.
//
// A destination that appears both quoted and unquoted is reported once, as
// unquoted: the author did use it themselves, and which occurrence came first
// in the text is not something a caller should depend on.
func Find(body string) []Reference {
	source := []byte(body)
	doc := goldmark.New().Parser().Parse(text.NewReader(source))

	var refs []Reference
	index := make(map[string]int)
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		var destination string
		switch node := n.(type) {
		case *ast.Link:
			destination = string(node.Destination)
		case *ast.Image:
			destination = string(node.Destination)
		case *ast.AutoLink:
			// `<https://example.com>` renders as a clickable link exactly like an
			// inline one, so it is the same thing to every caller here.
			destination = string(node.URL(source))
		default:
			return ast.WalkContinue, nil
		}

		quoted := hasBlockquoteAncestor(n)
		if at, seen := index[destination]; seen {
			// Downgrade only: one unquoted use is enough to make it the author's.
			if !quoted {
				refs[at].Quoted = false
			}
			return ast.WalkContinue, nil
		}
		index[destination] = len(refs)
		refs = append(refs, Reference{Destination: destination, Quoted: quoted})
		return ast.WalkContinue, nil
	})
	return refs
}

// hasBlockquoteAncestor walks the full ancestor chain rather than checking the
// immediate parent, because a link is normally nested a few containers deep —
// inside a paragraph, often inside a list item — before the blockquote appears.
func hasBlockquoteAncestor(n ast.Node) bool {
	for p := n.Parent(); p != nil; p = p.Parent() {
		if p.Kind() == ast.KindBlockquote {
			return true
		}
	}
	return false
}
