package handler

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func lintUUID(b byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{b}, Valid: true} }

func TestLintAgentHidden(t *testing.T) {
	owner, other := lintUUID(1), lintUUID(2)
	otherID := uuidToString(other)
	privAgent := db.Agent{OwnerID: owner, PermissionMode: "private"}
	pubAgent := db.Agent{OwnerID: owner, PermissionMode: "public_to"}
	pubTargets := []db.AgentInvocationTarget{{TargetType: "workspace"}}
	publicRT := db.AgentRuntime{OwnerID: owner, Visibility: "public"}
	privRT := db.AgentRuntime{OwnerID: owner, Visibility: "private"}
	sharedRT := db.AgentRuntime{OwnerID: owner, Visibility: "private", SharedWith: []pgtype.UUID{other}}

	cases := []struct {
		name    string
		agent   db.Agent
		targets []db.AgentInvocationTarget
		rt      db.AgentRuntime
		userID  string
		role    string
		want    bool
	}{
		{"member, private agent", privAgent, nil, publicRT, otherID, "member", true},
		{"member, shared agent, private runtime", pubAgent, pubTargets, privRT, otherID, "member", true},
		{"member, shared agent, public runtime", pubAgent, pubTargets, publicRT, otherID, "member", false},
		{"member, runtime shared with them", pubAgent, pubTargets, sharedRT, otherID, "member", false},
		{"agent owner", privAgent, nil, privRT, uuidToString(owner), "member", false},
		{"workspace admin", privAgent, nil, privRT, otherID, "admin", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := lintAgentHidden(c.agent, c.targets, c.rt, c.userID, c.role); got != c.want {
				t.Fatalf("hidden = %v, want %v", got, c.want)
			}
		})
	}
}
