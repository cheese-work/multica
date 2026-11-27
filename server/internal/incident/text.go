package incident

import (
	"fmt"
	"strings"
	"time"
)

// Texts carry no agent or member mention: a mention would start a run or ping a person.

func Title(class string) string { return "incident(provider): " + class }

// Body lists the error, counts and affected issues of the incident as it stands.
func Body(in Incident) string {
	var b strings.Builder
	last := in.Runs[0]
	for _, r := range in.Runs {
		if r.At.After(last.At) {
			last = r
		}
	}
	fmt.Fprintf(&b, "Error class: `%s`. Newest error (%s): `%s`\n\n", in.Class, last.At.UTC().Format(time.RFC3339), oneLine(last.Error, 300))
	fmt.Fprintf(&b, "%d failed runs on %d issues and %d agents in the last %d minutes.\n\nAffected issues:\n", len(in.Runs), len(in.Issues), len(in.Agents), int(Window.Minutes()))
	for _, id := range in.Issues {
		fmt.Fprintf(&b, "- [%s](mention://issue/%s)\n", id, id)
	}
	fmt.Fprintf(&b, "\nAffected agent IDs: %s\n", strings.Join(in.Agents, ", "))
	return b.String()
}

// DMText is the one direct message of an incident. issueLink is the incident issue as a mention link.
func DMText(in Incident, issueLink string) string {
	return fmt.Sprintf("Provider incident `%s`: %d failed runs on %d issues and %d agents in %d minutes. Record: %s. "+
		"Rerouting and retry are not automatic yet (CHE-10030), so affected work waits. No more messages until it closes; it closes itself after %d minutes with no new failure of this class.",
		in.Class, len(in.Runs), len(in.Issues), len(in.Agents), int(Window.Minutes()), issueLink, int(Window.Minutes()))
}

func CloseBody(class string, last time.Time) string {
	return fmt.Sprintf("Closed: no `%s` failure since %s (%d minutes). Retried: none. Automatic retry of failed runs is CHE-10030.",
		class, last.UTC().Format(time.RFC3339), int(Window.Minutes()))
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "`", "'")), " ")
	if len(s) > max {
		s = s[:max]
	}
	return s
}
