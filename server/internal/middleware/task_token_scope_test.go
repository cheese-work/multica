package middleware

import "testing"

func TestGovernanceProposalCapability(t *testing.T) {
	const caseID = "11111111-1111-4111-8111-111111111111"
	const attemptID = "22222222-2222-4222-8222-222222222222"
	base := "/api/governance/proposals/" + caseID + "/attempts/" + attemptID
	tests := []struct {
		name      string
		method    string
		path      string
		rawPath   string
		rawQuery  string
		caseID    string
		attemptID string
		want      string
	}{
		{name: "evidence read", method: "GET", path: base + "/evidence", caseID: caseID, attemptID: attemptID, want: "evidence"},
		{name: "heartbeat", method: "POST", path: base + "/heartbeat", caseID: caseID, attemptID: attemptID, want: "heartbeat"},
		{name: "single result submission", method: "POST", path: base + "/result", caseID: caseID, attemptID: attemptID, want: "result"},
		{name: "evidence write denied", method: "POST", path: base + "/evidence", caseID: caseID, attemptID: attemptID},
		{name: "heartbeat read denied", method: "GET", path: base + "/heartbeat", caseID: caseID, attemptID: attemptID},
		{name: "unlisted endpoint denied", method: "GET", path: "/api/issues/" + caseID, caseID: caseID, attemptID: attemptID},
		{name: "route alias denied", method: "POST", path: "/api/tasks/" + attemptID + "/progress", caseID: caseID, attemptID: attemptID},
		{name: "path query denied", method: "GET", path: base + "/evidence", rawQuery: "case_id=" + caseID, caseID: caseID, attemptID: attemptID},
		{name: "encoded path denied", method: "GET", path: base + "/evidence", rawPath: base + "%2Fevidence", caseID: caseID, attemptID: attemptID},
		{name: "trailing slash denied", method: "GET", path: base + "/evidence/", caseID: caseID, attemptID: attemptID},
		{name: "case binding enforced", method: "GET", path: base + "/evidence", caseID: "33333333-3333-4333-8333-333333333333", attemptID: attemptID},
		{name: "attempt binding enforced", method: "POST", path: base + "/result", caseID: caseID, attemptID: "44444444-4444-4444-8444-444444444444"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := governanceProposalCapability(test.method, test.path, test.rawPath, test.rawQuery, test.caseID, test.attemptID)
			if got != test.want {
				t.Fatalf("capability = %q, want %q", got, test.want)
			}
		})
	}
}
