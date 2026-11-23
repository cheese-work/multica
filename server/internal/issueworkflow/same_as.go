package issueworkflow

// validateSameAs checks a same_as handler against its workflow: it names
// another step of the workflow, and following same_as steps from there never
// comes back to this one (CHE-9114). The walk stops at s before it reads s's
// stored entry, so a step being saved is judged as it will be stored.
func validateSameAs(s Step, def Definition) error {
	target := s.Handler.Step
	switch {
	case target == "":
		return invalid("choose the step %q takes its agent from", s.StatusKey)
	case target == s.StatusKey:
		return invalid("%q cannot take its agent from itself", s.StatusKey)
	case !def.Has(target):
		return invalid("%q is not a status of this workflow", target)
	}
	seen := map[string]bool{s.StatusKey: true}
	for key := target; ; {
		if seen[key] {
			return invalid("%q takes its agent from %q, which leads back to it", s.StatusKey, target)
		}
		seen[key] = true
		next, ok := def.Step(key)
		if !ok || next.Handler.Type != HandlerSameAs {
			return nil
		}
		key = next.Handler.Step
	}
}
