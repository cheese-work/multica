package service

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestPublishChatCompactingCarriesSessionOwner(t *testing.T) {
	bus := events.New()
	var got []events.Event
	bus.Subscribe(protocol.EventChatSessionUpdated, func(e events.Event) { got = append(got, e) })
	svc := &TaskService{Bus: bus}
	creator := util.MustParseUUID("11111111-1111-4111-8111-111111111111")
	session := db.ChatSession{ID: util.MustParseUUID("22222222-2222-4222-8222-222222222222"), CreatorID: creator}

	svc.publishChatCompacting("ws", session, true)

	if len(got) != 1 || got[0].ChatSessionOwnerID != util.UUIDToString(creator) {
		t.Fatalf("compacting event owner = %+v, want %s", got, util.UUIDToString(creator))
	}
}
