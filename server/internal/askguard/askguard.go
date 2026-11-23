// Package askguard stores ask guard events and reads the workspace mode that
// decides what a hit does. The rule it serves: natural language never drives a
// trigger; only typed fields and structural links do.
package askguard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/metrics"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// SettingKey is the workspace.settings key that holds the mode.
const SettingKey = "ask_guard_mode"

// ReversibleSettingKey holds the mode of the reversible-ask guard (CHE-5754):
// a plain agent question to a person about work Standing Delivery Authorization
// already allows. Its own key, so it reverts without touching SettingKey.
const ReversibleSettingKey = "reversible_ask_guard"

// WaitingAudienceSettingKey holds the mode of S4 (CHE-7289): an agent comment
// on a waiting issue must carry a typed audience. Its own key, so S4 starts in
// warn and reverts without touching SettingKey.
const WaitingAudienceSettingKey = "waiting_audience_guard"

// WaitingOnSettingKey holds the mode of S5 (CHE-7290): an agent's move into a
// waiting status must say who acts next. Its own key, so S5 starts in warn and
// reverts without touching SettingKey or WaitingAudienceSettingKey.
const WaitingOnSettingKey = "waiting_on_guard"

type Mode string

const (
	ModeOff   Mode = "off"
	ModeWarn  Mode = "warn"
	ModeBlock Mode = "block"
	// ModeMove marks an operation the server routed instead of refusing (the
	// long-thread route, CHE-6432). Never a workspace setting, so ParseMode
	// does not accept it; the recovery and repair workers read block rows only.
	ModeMove Mode = "move"
)

// ParseMode accepts exactly the three mode names.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case ModeOff, ModeWarn, ModeBlock:
		return m, nil
	}
	return "", fmt.Errorf("%s must be off, warn or block", SettingKey)
}

// ModeFromSettings reads the mode from a workspace.settings value. A missing
// key is the upstream default, off. A malformed value is an error: callers
// must not guess a mode.
func ModeFromSettings(raw []byte) (Mode, error) { return modeFromSettings(raw, SettingKey) }

// ReversibleModeFromSettings reads ReversibleSettingKey the same way.
func ReversibleModeFromSettings(raw []byte) (Mode, error) {
	return modeFromSettings(raw, ReversibleSettingKey)
}

// WaitingAudienceModeFromSettings reads WaitingAudienceSettingKey the same way.
func WaitingAudienceModeFromSettings(raw []byte) (Mode, error) {
	return modeFromSettings(raw, WaitingAudienceSettingKey)
}

// WaitingOnModeFromSettings reads WaitingOnSettingKey the same way.
func WaitingOnModeFromSettings(raw []byte) (Mode, error) {
	return modeFromSettings(raw, WaitingOnSettingKey)
}

func modeFromSettings(raw []byte, key string) (Mode, error) {
	if len(raw) == 0 {
		return ModeOff, nil
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		return "", fmt.Errorf("workspace settings: %w", err)
	}
	v, ok := settings[key]
	if !ok {
		return ModeOff, nil
	}
	s, ok := v.(string)
	if !ok {
		return ParseMode("")
	}
	return ParseMode(s)
}

// Op identifies one agent operation that hit the guard. Its key is the task,
// the kind and the SHA256 of the request payload, so a retry of the same
// operation in the same task finds the same event while the same text in a
// later task is a new event.
//
// WarnOnly records the event as warn even when the workspace is in block mode,
// for operations that must never be refused or recovered (a chat-lane task, or
// an agent call with no task).
//
// MemberID is the one member the operation asks, when it names exactly one. A
// request created with --recovers clears the event only for that member.
type Op struct {
	IssueID, TaskID, MemberID pgtype.UUID
	Kind                      string
	Payload                   []byte
	WarnOnly                  bool
	// Source is the text a recovery notice quotes verbatim. Empty when the
	// operation has no ask text (a wakeup), which sends the event to repair.
	Source string
}

// Guard writes events through Queries. Queries must not be bound to the
// caller's transaction: the event commits on its own, before a refusal rolls
// the operation back.
type Guard struct {
	Queries *db.Queries
	Metrics *metrics.BusinessMetrics
}

// Verdict is the outcome of a hit. Event is nil when the mode is off. The
// event is always the one created under the current mode, so a refusal is
// backed by a block event while an earlier warn event of the same operation
// keeps its snapshot.
type Verdict struct {
	Event *db.AskGuardEvent
	Block bool
}

// Hit records the operation as a guard event unless the workspace mode is off.
// An error means nothing was written and the caller must not run the operation.
func (g Guard) Hit(ctx context.Context, workspaceID pgtype.UUID, op Op) (Verdict, error) {
	ws, err := g.Queries.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return Verdict{}, fmt.Errorf("read workspace: %w", err)
	}
	mode, err := ModeFromSettings(ws.Settings)
	if err != nil {
		return Verdict{}, err
	}
	if mode == ModeOff {
		return Verdict{}, nil
	}
	if op.WarnOnly {
		mode = ModeWarn
	}
	ev, result, err := g.record(ctx, workspaceID, mode, op)
	g.Metrics.RecordAskGuardEvent(string(mode), result)
	if err != nil {
		return Verdict{}, err
	}
	return Verdict{Event: &ev, Block: mode == ModeBlock}, nil
}

// RecordWarn stores the operation as a warn event whatever the workspace
// ask_guard_mode is, for a rule that has its own mode (S4). The event is never
// a block event, so the recovery pass, which reads block events, never acts on
// it: the refusal itself is the fix.
func (g Guard) RecordWarn(ctx context.Context, workspaceID pgtype.UUID, op Op) error {
	_, result, err := g.record(ctx, workspaceID, ModeWarn, op)
	g.Metrics.RecordAskGuardEvent(string(ModeWarn), result)
	return err
}

// RecordNotice stores a typed ask (--ask comment, --waiting-on member move) as
// a block event, the kind the recovery pass delivers once its run ends. Unlike
// RecordWarn, Queries may be bound to the caller's transaction, so the event
// commits with the comment it serves. The operation key makes a repeat of the
// same payload a no-op. It writes no metric: it is not a guard hit.
func (g Guard) RecordNotice(ctx context.Context, workspaceID pgtype.UUID, op Op) error {
	_, _, err := g.record(ctx, workspaceID, ModeBlock, op)
	return err
}

// RecordMove stores a routed operation as a move event. Like RecordNotice,
// Queries may be bound to the caller's transaction, so the event commits with
// the comment it describes. It writes no metric: it is not a guard hit.
func (g Guard) RecordMove(ctx context.Context, workspaceID pgtype.UUID, op Op) error {
	_, _, err := g.record(ctx, workspaceID, ModeMove, op)
	return err
}

func (g Guard) record(ctx context.Context, workspaceID pgtype.UUID, mode Mode, op Op) (db.AskGuardEvent, string, error) {
	sum := sha256.Sum256(op.Payload)
	digest := hex.EncodeToString(sum[:])
	ev, err := g.Queries.InsertAskGuardEvent(ctx, db.InsertAskGuardEventParams{
		ID: dbid.NewV7(), WorkspaceID: workspaceID, IssueID: op.IssueID, TaskID: op.TaskID,
		OperationKind: op.Kind, PayloadSha256: digest, Mode: string(mode), MemberID: op.MemberID,
		SourceText: pgtype.Text{String: op.Source, Valid: op.Source != ""},
	})
	if err == nil {
		return ev, "created", nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ev, "error", fmt.Errorf("write guard event: %w", err)
	}
	ev, err = g.Queries.GetAskGuardEventByOperation(ctx, db.GetAskGuardEventByOperationParams{
		TaskID: op.TaskID, OperationKind: op.Kind, PayloadSha256: digest, Mode: string(mode),
	})
	if err != nil {
		return ev, "error", fmt.Errorf("read guard event: %w", err)
	}
	return ev, "retry", nil
}

// MentionedMembers returns the member IDs that content mentions with a
// structural link. It uses the parser that drives notification dispatch, so a
// link inside a code span or fence counts exactly as it notifies today, and an
// ID that is not a UUID (which dispatch skips) does not count.
func MentionedMembers(content string) []string {
	var ids []string
	for _, m := range util.ParseMentions(content) {
		if m.Type != "member" {
			continue
		}
		if _, err := util.ParseUUID(m.ID); err == nil {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// OwnerScriptSettingKey holds the mode of the owner-script guard (CHE-7138).
const OwnerScriptSettingKey = "owner_script_guard"

// OwnerScriptModeFromSettings reads OwnerScriptSettingKey. Unlike the other ask
// guards a missing key is warn, not off: the guard records and never refuses
// until a person sets block.
func OwnerScriptModeFromSettings(raw []byte) (Mode, error) {
	if len(raw) == 0 {
		return ModeWarn, nil
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		return "", fmt.Errorf("workspace settings: %w", err)
	}
	v, ok := settings[OwnerScriptSettingKey]
	if !ok {
		return ModeWarn, nil
	}
	s, _ := v.(string)
	return ParseMode(s)
}
