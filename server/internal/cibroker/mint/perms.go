package mint

// Permissions maps a GitHub App permission name to "read", "write" or "admin".
type Permissions map[string]string

var permLevels = [...]string{"", "read", "write", "admin"}

func permRank(level string) int {
	for i := 1; i < len(permLevels); i++ {
		if permLevels[i] == level {
			return i
		}
	}
	return 0 // unknown level: grants nothing
}

// Intersect returns the permissions every one of wanted, entryMax and ceiling
// grants, at the lowest level among them. An empty wanted means "everything
// the entry allows". Names missing from any input, and unknown levels, are
// dropped.
func Intersect(wanted, entryMax, ceiling Permissions) Permissions {
	if len(wanted) == 0 {
		wanted = entryMax
	}
	out := Permissions{}
	for name, w := range wanted {
		if r := min(permRank(w), permRank(entryMax[name]), permRank(ceiling[name])); r > 0 {
			out[name] = permLevels[r]
		}
	}
	return out
}
