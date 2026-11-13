package guard

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Scope ladder, least to most specific: the workspace (the Policy's own
// fields), then org, repo and path layers. A deeper layer overrides a shallower
// one key by key. A locked layer freezes the keys it sets: no deeper layer
// changes them, and the least specific lock on a key wins. Keys are
// "plugin:<name>", "review:<repo pattern>" and "verify".
const (
	ScopeOrg  = "org"
	ScopeRepo = "repo"
	ScopePath = "path"
)

// Layer is one rung below the workspace. Org layers set Org (owner), repo
// layers Repo (owner/name), path layers Repo plus Path, a directory prefix
// inside the repository.
type Layer struct {
	Scope   string       `json:"scope"`
	Org     string       `json:"org,omitempty"`
	Repo    string       `json:"repo,omitempty"`
	Path    string       `json:"path,omitempty"`
	Locked  bool         `json:"locked,omitempty"`
	Verify  *bool        `json:"verify,omitempty"`
	Plugins []Plugin     `json:"plugins,omitempty"`
	Review  []ReviewRule `json:"review,omitempty"`
}

// Target is where a call runs: Repo is owner/name and Path is relative to the
// repository root. Both may be empty, which selects the workspace layer only.
type Target struct{ Repo, Path string }

func (l *Layer) rank() int { return map[string]int{ScopeOrg: 1, ScopeRepo: 2, ScopePath: 3}[l.Scope] }

func (l *Layer) applies(t Target) bool {
	owner, _, _ := strings.Cut(t.Repo, "/")
	switch l.Scope {
	case ScopeOrg:
		return owner != "" && strings.EqualFold(owner, l.Org)
	case ScopeRepo:
		return t.Repo != "" && strings.EqualFold(t.Repo, l.Repo)
	case ScopePath:
		p := path.Clean(t.Path)
		return t.Repo != "" && strings.EqualFold(t.Repo, l.Repo) && (p == l.Path || strings.HasPrefix(p, l.Path+"/"))
	}
	return false
}

func (l *Layer) validate() error {
	hasSlash := func(s string) bool { o, n, ok := strings.Cut(s, "/"); return ok && o != "" && n != "" }
	switch l.Scope {
	case ScopeOrg:
		if l.Org == "" || strings.Contains(l.Org, "/") {
			return fmt.Errorf("org layer needs org (owner)")
		}
	case ScopeRepo:
		if !hasSlash(l.Repo) {
			return fmt.Errorf("repo layer needs repo as owner/name")
		}
	case ScopePath:
		if !hasSlash(l.Repo) || l.Path == "" {
			return fmt.Errorf("path layer needs repo as owner/name and a path")
		}
		if c := path.Clean(l.Path); c != l.Path || path.IsAbs(c) || c == "." || c == ".." || strings.HasPrefix(c, "../") {
			return fmt.Errorf("path layer path %q must be a clean relative directory", l.Path)
		}
	default:
		return fmt.Errorf("unknown layer scope %q", l.Scope)
	}
	sub := Policy{Version: 1, Plugins: l.Plugins, Review: l.Review}
	return sub.validate()
}

// Resolve flattens the ladder for t into a Policy without layers.
func (p *Policy) Resolve(t Target) *Policy {
	var layers []Layer
	for _, l := range p.Layers {
		if l.applies(t) {
			layers = append(layers, l)
		}
	}
	sort.SliceStable(layers, func(i, j int) bool {
		if layers[i].rank() != layers[j].rank() {
			return layers[i].rank() < layers[j].rank()
		}
		return len(layers[i].Path) < len(layers[j].Path)
	})
	layers = append([]Layer{{Locked: p.Locked, Verify: p.Verify, Plugins: p.Plugins, Review: p.Review}}, layers...)

	out := &Policy{Version: p.Version}
	frozen := map[string]bool{}
	var plugins []string // plugin names in first-seen order
	byPlugin := map[string]Plugin{}
	var reviews, lockedReviews []string
	byReview := map[string]ReviewRule{}
	set := func(key string, locked bool, apply func()) {
		if frozen[key] {
			return
		}
		apply()
		frozen[key] = locked
	}
	for _, l := range layers {
		if l.Verify != nil {
			set("verify", l.Locked, func() { v := *l.Verify; out.Verify = &v })
		}
		for _, pl := range l.Plugins {
			set("plugin:"+pl.Name, l.Locked, func() {
				if _, ok := byPlugin[pl.Name]; !ok {
					plugins = append(plugins, pl.Name)
				}
				byPlugin[pl.Name] = pl
			})
		}
		for _, r := range l.Review {
			if coveredBy(lockedReviews, r.Repo) {
				continue
			}
			k := strings.ToLower(r.Repo)
			if _, ok := byReview[k]; !ok {
				reviews = append(reviews, k)
			}
			byReview[k] = r
		}
		if l.Locked { // a lock covers every repository its patterns match, not only the same string
			for _, r := range l.Review {
				lockedReviews = append(lockedReviews, r.Repo)
			}
		}
	}
	for _, n := range plugins {
		out.Plugins = append(out.Plugins, byPlugin[n])
	}
	for _, k := range reviews {
		out.Review = append(out.Review, byReview[k])
	}
	return out
}

// coveredBy reports whether a locked review pattern in locked matches every
// repository pattern q selects: the same pattern, "*", or owner/* over q under
// that owner.
func coveredBy(locked []string, q string) bool {
	qOwner, _, _ := strings.Cut(q, "/")
	for _, p := range locked {
		pOwner, pName, _ := strings.Cut(p, "/")
		if strings.EqualFold(p, q) || p == "*" || (pName == "*" && strings.EqualFold(pOwner, qOwner)) {
			return true
		}
	}
	return false
}
