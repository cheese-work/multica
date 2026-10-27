package protocol

type DecisionLane struct {
	ID            string                 `json:"id"`
	Title         string                 `json:"title"`
	UpdatedAt     string                 `json:"updated_at"`
	IssueMentions []DecisionIssueMention `json:"issue_mentions"`
	// Eligible: the issue was discussed in this DM, so a request for it may go here.
	Eligible bool `json:"eligible"`
}

type DecisionIssueMention struct {
	IssueID      string `json:"issue_id"`
	IssueKey     string `json:"issue_key"`
	IssueTitle   string `json:"issue_title"`
	MentionedAt  string `json:"mentioned_at"`
	RelativeTime string `json:"relative_time"`
	Snippet      string `json:"snippet"`
}
