package guard

import "strings"

// A flag found in an argument list. Forms: --name v, --name=v, -n v, -n=v, -nv.
type flagTok struct {
	name   string // long name without dashes
	value  string
	hasVal bool
	idx    int // index of the token that holds the flag (value may be at idx+1)
	sep    bool
	short  bool
	attach bool // -nv
}

type flagSpec struct {
	valued map[string]bool // long names that take a value
	short  map[byte]string // short letter -> long name
}

var prSpec = flagSpec{
	valued: set("title", "body", "body-file", "base", "head", "repo", "reviewer", "assignee", "label", "milestone", "project",
		"template", "add-reviewer", "remove-reviewer", "add-assignee", "remove-assignee", "add-label", "remove-label",
		"add-project", "remove-project", "json", "jq"),
	short: map[byte]string{'t': "title", 'b': "body", 'F': "body-file", 'B': "base", 'H': "head", 'R': "repo", 'r': "reviewer",
		'a': "assignee", 'l': "label", 'm': "milestone", 'p': "project", 'T': "template", 'd': "draft", 'f': "fill", 'w': "web", 'q': "jq"},
}

var apiSpec = flagSpec{
	valued: set("method", "header", "field", "raw-field", "input", "jq", "template", "hostname", "cache", "preview"),
	short:  map[byte]string{'X': "method", 'H': "header", 'f': "raw-field", 'F': "field", 'q': "jq", 't': "template", 'p': "preview"},
}

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// scan splits args into flags and positional words. It follows pflag closely
// enough for the guarded gh commands; the first "--" ends flag parsing.
func (s flagSpec) scan(args []string) (flags []flagTok, pos []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return flags, append(pos, args[i+1:]...)
		case strings.HasPrefix(a, "--"):
			name, val, eq := strings.Cut(a[2:], "=")
			t := flagTok{name: name, idx: i, value: val, hasVal: eq}
			if !eq && s.valued[name] && i+1 < len(args) {
				t.value, t.hasVal, t.sep = args[i+1], true, true
				i++
			}
			flags = append(flags, t)
		case len(a) > 1 && a[0] == '-':
			long, ok := s.short[a[1]]
			if !ok {
				flags = append(flags, flagTok{name: a[1:], idx: i, short: true})
				continue
			}
			t := flagTok{name: long, idx: i, short: true}
			switch {
			case !s.valued[long] && len(a) > 2:
				// a cluster of boolean shorts such as -dw
				for _, c := range []byte(a[1:]) {
					if n, ok := s.short[c]; ok && !s.valued[n] {
						flags = append(flags, flagTok{name: n, idx: i, short: true})
					}
				}
				continue
			case !s.valued[long]:
			case len(a) > 2 && a[2] == '=':
				t.value, t.hasVal = a[3:], true
			case len(a) > 2:
				t.value, t.hasVal, t.attach = a[2:], true, true
			case i+1 < len(args):
				t.value, t.hasVal, t.sep = args[i+1], true, true
				i++
			}
			flags = append(flags, t)
		default:
			pos = append(pos, a)
		}
	}
	return flags, pos
}

func find(flags []flagTok, name string) (flagTok, bool) {
	for _, f := range flags {
		if f.name == name {
			return f, true
		}
	}
	return flagTok{}, false
}

// values returns every value of a repeatable flag, splitting on commas.
func values(flags []flagTok, name string) []string {
	var out []string
	for _, f := range flags {
		if f.name == name && f.hasVal {
			for _, v := range strings.Split(f.value, ",") {
				if v = strings.TrimSpace(v); v != "" {
					out = append(out, v)
				}
			}
		}
	}
	return out
}

// isOn reports whether a boolean flag is set true; --name=false is off.
func isOn(flags []flagTok, name string) bool {
	f, ok := find(flags, name)
	if !ok {
		return false
	}
	return !f.hasVal || f.value != "false"
}

// withValue returns a copy of args with flag f's value replaced.
func withValue(args []string, f flagTok, v string) []string {
	out := append([]string(nil), args...)
	switch {
	case f.sep:
		out[f.idx+1] = v
	case f.attach:
		out[f.idx] = out[f.idx][:2] + v
	case f.short:
		out[f.idx] = out[f.idx][:2] + "=" + v
	default:
		out[f.idx] = "--" + f.name + "=" + v
	}
	return out
}

// leadingWords returns the words before the first flag: "pr create".
func leadingWords(args []string) []string {
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			return args[:i]
		}
	}
	return args
}
