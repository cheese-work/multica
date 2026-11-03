package service

import (
	"testing"
)

// S1-fire in each language: a member comment that fires an observe rule is
// audited the same way, because the audit reads the actor, not the text.
func TestIssueWakeupObserveFireAuditMatrix(t *testing.T) {
	instructions := map[string]string{
		"english":    "When the owner replies, decide whether to waive or rewrite it.",
		"vietnamese": "Khi chủ sở hữu trả lời, hãy quyết định bỏ qua hay viết lại.",
		"chinese":    "负责人回复后，决定是豁免还是重写。",
	}
	replies := map[string]string{"english": "Waive it.", "vietnamese": "Bỏ qua nhé.", "chinese": "豁免。"}
	for lang, instruction := range instructions {
		t.Run(lang, func(t *testing.T) {
			f, s, issue, agent := wakeFixture(t)
			f.Exec(t, `UPDATE workspace SET settings = '{"ask_guard_mode":"block"}' WHERE id = $1`, f.WorkspaceID)
			f.Cleanup(t, "DELETE FROM ask_guard_event WHERE issue_id=$1", issue)
			w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", EventTypes: []string{"comment.created"}, Purpose: "observe", Instruction: instruction})
			f.Exec(t, `INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type) VALUES($1,$2,'member',$3,$4,'comment')`, issue, f.WorkspaceID, f.UserID, replies[lang])
			wakeDispatch(t, s, w)
			if n := f.Count(t, "SELECT count(*) FROM ask_guard_event WHERE issue_id=$1 AND operation_kind='wakeup_observe_fire' AND mode='warn'", issue); n != 1 {
				t.Fatalf("member-caused fire audits = %d, want 1", n)
			}
		})
	}
}
