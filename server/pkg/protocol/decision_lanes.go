package protocol

type DecisionLane struct {
	ID            string                 `json:"id"`
	Title         string                 `json:"title"`
	UpdatedAt     string                 `json:"updated_at"`
	IssueMentions []DecisionIssueMention `json:"issue_mentions"`
	// Eligible is true when the DM names the requested issue; only eligible
	// DMs accept a decision request for it. ReferencedAt is the newest time it did.
	Eligible     bool   `json:"eligible"`
	ReferencedAt string `json:"referenced_at,omitempty"`
}

type DecisionIssueMention struct {
	IssueID      string `json:"issue_id"`
	IssueKey     string `json:"issue_key"`
	IssueTitle   string `json:"issue_title"`
	MentionedAt  string `json:"mentioned_at"`
	RelativeTime string `json:"relative_time"`
	Snippet      string `json:"snippet"`
}
